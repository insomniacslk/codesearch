package codesearch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v76/github"
	"github.com/sirupsen/logrus"
)

// Github implements the Backend interface
type Github struct {
	name        string
	apiEndpoint string
	token       string
	org         string
	cache       *githubCache
}

func NewGithub(name string, params BackendParams) (*Github, error) {
	org := params.GetString("org")
	if org == nil {
		return nil, fmt.Errorf("missing 'org' parameter")
	}
	token, err := params.ResolveToken()
	if err != nil {
		return nil, err
	}
	apiEndpoint := params.GetString("api_endpoint")
	if apiEndpoint == nil {
		return nil, fmt.Errorf("missing 'api_endpoint' parameter")
	}
	cache, err := newGithubCache(params)
	if err != nil {
		return nil, err
	}
	return &Github{
		cache:       cache,
		name:        name,
		org:         *org,
		token:       token,
		apiEndpoint: *apiEndpoint,
	}, nil
}

func (g *Github) Name() string {
	return g.name
}

func (g *Github) Org() string {
	return g.org
}

func (g *Github) Type() BackendType {
	return BackendTypeGithub
}

// Supports implements Backend. The GitHub search API exposes neither a
// case-sensitivity switch nor a file-name-only mode.
func (g *Github) Supports(Capability) bool {
	return false
}

func (g *Github) Search(ctx context.Context, terms string, opts SearchOptions) (Results, error) {
	searchstring := terms
	if g.org != "" {
		searchstring = "org:" + g.org + " " + terms
	}
	u, err := url.Parse(g.apiEndpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse GitHub API endpoint: %w", err)
	}
	client := github.NewClient(nil).WithAuthToken(g.token)
	if u.Host != "api.github.com" {
		client, err = client.WithEnterpriseURLs(g.apiEndpoint, g.apiEndpoint)
		if err != nil {
			return nil, fmt.Errorf("failed to configure GitHub Enterprise URLs: %w", err)
		}
	}
	ctx, cancel := searchContext(ctx, opts.Timeout)
	defer cancel()

	// The code search quota is small (10 requests a minute at the time of
	// writing) and GitHub returns at most 1000 hits for a query, so the largest
	// page size is what lets a full search fit in one quota window.
	sopts := github.SearchOptions{
		TextMatch:   true,
		ListOptions: github.ListOptions{PerPage: githubSearchPageSize},
	}
	var csresults []*github.CodeResult
	for {
		var (
			results  *github.CodeSearchResult
			response *github.Response
		)
		err := retry(ctx, func() error {
			var err error
			results, response, err = client.Search.Code(ctx, searchstring, &sopts)
			logrus.Debugf("Response: %+v", response)
			return err
		})
		if err != nil {
			if len(csresults) > 0 && githubIsRateLimit(err) {
				// the pages fetched so far are still good, and dropping them
				// would leave a broad search with no way to ever succeed
				logrus.Warnf("Results are incomplete, rate limited after %d hits: %v", len(csresults), err)
				break
			}
			return nil, fmt.Errorf("search failed: %w", err)
		}
		csresults = append(csresults, results.CodeResults...)
		if response.NextPage == 0 {
			break
		}
		if opts.Limit > 0 && uint(len(csresults)) >= opts.Limit {
			logrus.Debugf("Stopping after %d hits, enough for a limit of %d", len(csresults), opts.Limit)
			break
		}
		sopts.Page = response.NextPage
	}
	return g.toResult(ctx, client, csresults, opts)
}

// githubSearchPageSize is the largest page size the search API accepts.
const githubSearchPageSize = 100

// githubIsRateLimit reports whether err is a primary or secondary rate limit.
func githubIsRateLimit(err error) bool {
	var rlErr *github.RateLimitError
	var abuseErr *github.AbuseRateLimitError
	return errors.As(err, &rlErr) || errors.As(err, &abuseErr)
}

const maxAttempts = 3

// retry runs fn until it succeeds, until it fails with something not worth
// retrying, or until maxAttempts have been made. Pagination must not be driven
// from here: mixing the two is what previously let a permanently rate limited
// search spin forever without ever returning an error.
func retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; ; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err = fn(); err == nil {
			return nil
		}
		delay, retryable := retryDelay(err)
		if !retryable {
			return err
		}
		if attempt >= maxAttempts {
			return fmt.Errorf("giving up after %d attempts: %w", maxAttempts, err)
		}
		logrus.Warnf("Throttled, waiting %s before retry %d/%d", delay.Round(time.Second), attempt+1, maxAttempts)
		if serr := sleep(ctx, delay); serr != nil {
			return serr
		}
	}
}

// defaultSecondaryRateLimitDelay is what to wait when GitHub signals a
// secondary rate limit without saying for how long. Their guidance is to wait
// at least a minute; --timeout is the way to bound that.
const defaultSecondaryRateLimitDelay = time.Minute

// retryDelay reports how long to wait before retrying err, and whether err is
// worth retrying at all.
//
// Primary rate limits are not. They mean the quota for a whole window is spent,
// and the window is long compared to a single command: 30 requests a minute for
// code search, an hour for the core API. Waiting one out would hang the search
// for minutes and then spend the fresh quota on the same query that exhausted
// it, so it is reported immediately and the caller can decide when to come
// back. The error carries the reset time.
//
// Secondary limits are the opposite case: short-lived throttling with an
// explicit Retry-After, which is exactly what a retry is for.
func retryDelay(err error) (time.Duration, bool) {
	var abuseErr *github.AbuseRateLimitError
	if errors.As(err, &abuseErr) {
		if abuseErr.RetryAfter == nil {
			return defaultSecondaryRateLimitDelay, true
		}
		return max(*abuseErr.RetryAfter, 0), true
	}
	return 0, false
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// githubFile is a downloaded file, or the reason it could not be downloaded.
// The failure is kept so that a file with several matches is not requested
// again for each of them.
type githubFile struct {
	text  string
	lines []string
	err   error
}

// githubDownloadConcurrency is how many files are downloaded at once. It is
// deliberately modest: GitHub answers a burst of concurrent requests with a
// secondary rate limit.
const githubDownloadConcurrency = 8

// githubFileRef identifies a file to download.
type githubFileRef struct {
	owner, repo, path string
	// sha is the git blob SHA of the version of the file that the search
	// index holds
	sha string
}

func (r githubFileRef) String() string {
	// a GitHub identifier, not a file system path
	return fmt.Sprintf("%s/%s/%s", r.owner, r.repo, r.path)
}

// githubDownload is a file download that may still be in flight. file is set
// before done is closed.
type githubDownload struct {
	done chan struct{}
	file *githubFile
}

// githubDownloader downloads the files of a search ahead of the loop that
// turns hits into results. Code search reports every match in a file as a
// separate text match, and the API is rate limited hard enough that each file
// must be downloaded once only, so a download is shared by every match in the
// file. All methods must be called from the same goroutine.
type githubDownloader struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	fetch  func(ctx context.Context, ref githubFileRef) (string, error)
	// queue is every file in the order the results need them, without
	// duplicates; queue[:started] have been requested
	queue     []githubFileRef
	started   int
	downloads map[githubFileRef]*githubDownload
}

func newGithubDownloader(ctx context.Context, queue []githubFileRef, fetch func(context.Context, githubFileRef) (string, error)) *githubDownloader {
	ctx, cancel := context.WithCancel(ctx)
	return &githubDownloader{
		ctx:       ctx,
		cancel:    cancel,
		fetch:     fetch,
		queue:     queue,
		downloads: make(map[githubFileRef]*githubDownload, len(queue)),
	}
}

// startUpTo requests every file of the queue before index end that was not
// requested yet.
func (d *githubDownloader) startUpTo(end int) {
	for ; d.started < min(end, len(d.queue)); d.started++ {
		ref := d.queue[d.started]
		dl := &githubDownload{done: make(chan struct{})}
		d.downloads[ref] = dl
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer close(dl.done)
			text, err := d.fetch(d.ctx, ref)
			f := &githubFile{text: text, err: err}
			if err == nil {
				// split once per file rather than once per match
				f.lines = strings.Split(text, "\n")
			}
			dl.file = f
		}()
	}
}

// get waits for the download of a file that startUpTo requested.
func (d *githubDownloader) get(ref githubFileRef) *githubFile {
	dl, ok := d.downloads[ref]
	if !ok {
		return &githubFile{err: fmt.Errorf("internal error: %q was never requested", ref)}
	}
	<-dl.done
	return dl.file
}

// stop cancels the downloads still in flight and waits for them to end.
func (d *githubDownloader) stop() {
	d.cancel()
	d.wg.Wait()
}

// githubNeedsFile reports whether a text match is located in the file content,
// and so needs the file. A query that matches the file path yields a text
// match whose fragment is the path, which is not in the file content. A
// missing property is taken as content, which is what it is in practice.
func githubNeedsFile(tm *github.TextMatch) bool {
	p := tm.GetProperty()
	return p == "" || p == "content"
}

func githubRef(res *github.CodeResult) githubFileRef {
	return githubFileRef{
		owner: res.GetRepository().GetOwner().GetLogin(),
		repo:  res.GetRepository().GetName(),
		path:  res.GetPath(),
		sha:   res.GetSHA(),
	}
}

func (g *Github) fetchFile(ctx context.Context, client *github.Client, ref githubFileRef) (string, error) {
	owner, repo, path := ref.owner, ref.repo, ref.path
	fullPath := ref.String()
	var rc *github.RepositoryContent
	// The client remembers the last rate limit per category and refuses later
	// requests in that category without sending them. It derives the category
	// from the URL path, which does not work behind an Enterprise /api/v3
	// prefix: there an exhausted code search quota is filed under the core
	// API and would block every file download. Let the server decide instead.
	ctx = context.WithValue(ctx, github.BypassRateLimitCheck, true)
	err := retry(ctx, func() error {
		logrus.Debugf("Fetching file content, owner=%q repo=%q path=%q", owner, repo, path)
		var (
			resp *github.Response
			err  error
		)
		rc, _, resp, err = client.Repositories.GetContents(
			ctx, owner, repo, path, &github.RepositoryContentGetOptions{},
		)
		logrus.Debugf("Response: %+v", resp)
		logrus.Debugf("RepositoryContent: %+v", rc)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("failed to get content of file %q: %w", fullPath, err)
	}
	// GetContents returns a nil file content without an error when the path
	// turns out to be a directory.
	if rc == nil {
		return "", fmt.Errorf("no file content returned for %q", fullPath)
	}
	// GetContent decodes according to the reported encoding and reports the
	// >1MB "none" case, rather than dereferencing a Content that GitHub may
	// not have populated at all.
	text, err := rc.GetContent()
	if err != nil {
		return "", fmt.Errorf("failed to read content of file %q: %w", fullPath, err)
	}
	return text, nil
}

// githubAbortsSearch reports whether a failure to fetch one file must end the
// whole search. Cancellation and rate limits apply to every later request as
// well, so carrying on would only fail the same way for each remaining file.
// Anything else is specific to that file.
func githubAbortsSearch(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return githubIsRateLimit(err)
}

// githubMatchIndices returns the highlight of match relative to fragment, or
// false if the indices are missing or do not describe a range inside the
// fragment. They come from the API and are used to slice, so they cannot be
// trusted.
func githubMatchIndices(match *github.Match, fragment string) (int, int, bool) {
	// indices is documented as a start/end pair but is omitted entirely for
	// some match kinds
	if match == nil || len(match.Indices) < 2 {
		return 0, 0, false
	}
	i0, i1 := match.Indices[0], match.Indices[1]
	if i0 < 0 || i0 > i1 || i1 > len(fragment) {
		return 0, 0, false
	}
	return i0, i1, true
}

func (g *Github) toResult(ctx context.Context, client *github.Client, csresults []*github.CodeResult, opts SearchOptions) (Results, error) {
	var results Results
	// the files to download, in the order the loop below needs them, and the
	// position of the file of each hit in that order
	var queue []githubFileRef
	queued := make(map[githubFileRef]int)
	for _, res := range csresults {
		ref := githubRef(res)
		if _, ok := queued[ref]; ok {
			continue
		}
		for _, tm := range res.TextMatches {
			if githubNeedsFile(tm) {
				queued[ref] = len(queue)
				queue = append(queue, ref)
				break
			}
		}
	}
	downloader := newGithubDownloader(ctx, queue, func(ctx context.Context, ref githubFileRef) (string, error) {
		if text, ok := g.cache.load(ref.sha); ok {
			logrus.Debugf("Using cached content for %q", ref)
			return text, nil
		}
		text, err := g.fetchFile(ctx, client, ref)
		if err == nil {
			// stored under the SHA of what was downloaded, which is not
			// ref.sha when the file changed after GitHub indexed it
			g.cache.store(text)
		}
		return text, err
	})
	defer downloader.stop()
	for _, res := range csresults {
		// each hit costs a file download, so stop as soon as there are enough
		if opts.Limit > 0 && uint(len(results)) >= opts.Limit {
			break
		}
		ref := githubRef(res)
		if pos, ok := queued[ref]; ok {
			// Download this file and the ones after it at the same time. A
			// file gives at least one result as a rule, so with a limit no
			// more files are requested ahead than results are still missing.
			ahead := githubDownloadConcurrency
			if opts.Limit > 0 {
				ahead = min(ahead, int(opts.Limit)-len(results))
			}
			downloader.startUpTo(pos + ahead)
		}
		logrus.Debugf("Result:\n")
		logrus.Debugf("  Name: %s:\n", res.GetName())
		logrus.Debugf("  Path: %s:\n", res.GetPath())
		logrus.Debugf("  SHA: %s:\n", res.GetSHA())
		logrus.Debugf("  HTMLURL: %s:\n", res.GetHTMLURL())
		logrus.Debugf("  Repository: %+v:\n", res.Repository)
		logrus.Debugf("  TextMatches:\n")
		base := Result{
			Backend:  g.Name(),
			Path:     res.GetPath(),
			RepoURL:  res.GetRepository().GetHTMLURL(),
			FileURL:  res.GetHTMLURL(),
			Owner:    res.GetRepository().GetOwner().GetLogin(),
			RepoName: res.GetRepository().GetName(),
			Branch:   res.GetRepository().GetDefaultBranch(),
		}
		warned := false
		for idx, tm := range res.TextMatches {
			logrus.Debugf("    %d) text match:\n", idx+1)
			logrus.Debugf("        ObjectURL: %s\n", tm.GetObjectURL())
			logrus.Debugf("        ObjectType: %s\n", tm.GetObjectType())
			logrus.Debugf("        Property: %s\n", tm.GetProperty())
			logrus.Debugf("        Fragment: %s\n", tm.GetFragment())
			if !githubNeedsFile(tm) {
				logrus.Debugf("Skipping text match on property %q", tm.GetProperty())
				continue
			}
			fragment := tm.GetFragment()
			f := downloader.get(ref)
			fragmentStart := -1
			if f.err != nil {
				if githubAbortsSearch(ctx, f.err) {
					return nil, f.err
				}
				if !warned {
					logrus.Warnf("Showing only the search fragment for %q: %v", base.Path, f.err)
					warned = true
				}
			} else if fragmentStart = strings.Index(f.text, fragment); fragmentStart == -1 {
				// the search index is behind the file, e.g. it changed after
				// GitHub indexed it
				if !warned {
					logrus.Warnf("Showing only the search fragment for %q: fragment not found in the current file content", base.Path)
					warned = true
				}
			}
			logrus.Debugf("        Matches:\n")
			for _, match := range tm.Matches {
				i0, i1, ok := githubMatchIndices(match, fragment)
				if !ok {
					logrus.Debugf("Skipping match with missing or invalid indices")
					continue
				}
				var result Result
				if fragmentStart == -1 {
					result = fragmentResult(base, fragment, i0, i1, opts)
				} else {
					result, ok = fileResult(base, f, fragmentStart+i0, i1-i0, opts)
					if !ok {
						continue
					}
				}
				results = append(results, result)
			}
		}
	}
	return results, nil
}

// fileResult builds a result for a match that starts at byte start of the
// full file content.
func fileResult(base Result, f *githubFile, start, length int, opts SearchOptions) (Result, bool) {
	lineno := strings.Count(f.text[:start], "\n") + 1
	// start of the highlight, relative to the line rather than to the full
	// text
	startInLine := start - (strings.LastIndex(f.text[:start], "\n") + 1)
	fileURL, err := url.Parse(base.FileURL)
	if err != nil {
		logrus.Warnf("Skipping match in %q: invalid file URL %q: %v", base.Path, base.FileURL, err)
		return Result{}, false
	}
	fileURL.Fragment = fmt.Sprintf("L%d", lineno)
	before, after := contextLines(f.lines, lineno, opts.LinesBefore, opts.LinesAfter)
	r := base
	r.Line = f.lines[lineno-1]
	r.Lineno = lineno
	r.Context = ResultContext{Before: before, After: after}
	r.Highlight = [2]int{startInLine, startInLine + length}
	r.FileURL = fileURL.String()
	return r, true
}

// fragmentResult builds a result from the search fragment alone, for when the
// file content is not available or no longer holds the fragment. The line
// number within the file is unknown, so Lineno is 0 and the URL has no line
// anchor.
func fragmentResult(base Result, fragment string, i0, i1 int, opts SearchOptions) Result {
	lines := strings.Split(fragment, "\n")
	lineno := strings.Count(fragment[:i0], "\n") + 1
	lineStart := strings.LastIndex(fragment[:i0], "\n") + 1
	before, after := contextLines(lines, lineno, opts.LinesBefore, opts.LinesAfter)
	r := base
	r.Line = lines[lineno-1]
	r.Context = ResultContext{Before: before, After: after}
	r.Highlight = [2]int{i0 - lineStart, i1 - lineStart}
	return r
}
