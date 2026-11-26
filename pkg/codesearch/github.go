package codesearch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
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
	return &Github{
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

	sopts := github.SearchOptions{TextMatch: true}
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

// fileContent fetches a file, remembering what it downloaded in cache. Code
// search reports every match in a file as a separate text match, and the
// search API is rate limited hard enough that re-downloading the same file
// once per match is the difference between a search completing and hitting the
// limit.
func (g *Github) fileContent(ctx context.Context, client *github.Client, cache map[string]*githubFile, owner, repo, path string) *githubFile {
	fullPath := fmt.Sprintf("%s/%s/%s", owner, repo, path)
	if f, ok := cache[fullPath]; ok {
		logrus.Debugf("Using cached content for %q", fullPath)
		return f
	}
	text, err := g.fetchFile(ctx, client, owner, repo, path)
	f := &githubFile{text: text, err: err}
	if err == nil {
		// split once per file rather than once per match
		f.lines = strings.Split(text, "\n")
	}
	cache[fullPath] = f
	return f
}

func (g *Github) fetchFile(ctx context.Context, client *github.Client, owner, repo, path string) (string, error) {
	fullPath := fmt.Sprintf("%s/%s/%s", owner, repo, path)
	var rc *github.RepositoryContent
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
	var rlErr *github.RateLimitError
	var abuseErr *github.AbuseRateLimitError
	return errors.As(err, &rlErr) || errors.As(err, &abuseErr)
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
	contents := make(map[string]*githubFile)
	for _, res := range csresults {
		// each hit costs a file download, so stop as soon as there are enough
		if opts.Limit > 0 && uint(len(results)) >= opts.Limit {
			break
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
			// a query that matches the file path yields a text match whose
			// fragment is the path, which is not in the file content. A
			// missing property is taken as content, which is what it is in
			// practice.
			if p := tm.GetProperty(); p != "" && p != "content" {
				logrus.Debugf("Skipping text match on property %q", p)
				continue
			}
			fragment := tm.GetFragment()
			f := g.fileContent(ctx, client, contents,
				base.Owner, base.RepoName, base.Path,
			)
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
