package codesearch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-github/v60/github"
	"github.com/sirupsen/logrus"
)

// Github implements the Backend interface
type Github struct {
	name              string
	apiEndpoint       string
	token             string
	org               string
	linesBefore       int
	linesAfter        int
	caseInsensitive   bool
	searchInFilenames bool
	timeout           time.Duration
}

func (g *Github) New(name string, params BackendParams) (Backend, error) {
	org := params.GetString("org")
	if org == nil {
		return nil, fmt.Errorf("missing 'org' parameter")
	}
	token := params.GetString("token")
	if token == nil {
		return nil, fmt.Errorf("missing 'token' parameter")
	}
	apiEndpoint := params.GetString("api_endpoint")
	if apiEndpoint == nil {
		return nil, fmt.Errorf("missing 'apiEndpoint' parameter")
	}
	return &Github{
		name:        name,
		org:         *org,
		token:       *token,
		apiEndpoint: *apiEndpoint,
	}, nil
}

func (g *Github) Name() string {
	return g.name
}

func (g *Github) Org() string {
	return g.org
}

func (g *Github) Type() string {
	return BackendTypeGithub
}

func (g *Github) SetCaseInsensitive(v bool) {
	g.caseInsensitive = v
}

func (g *Github) SetLinesBefore(n int) {
	g.linesBefore = n
}

func (g *Github) SetLinesAfter(n int) {
	g.linesAfter = n
}

func (g *Github) SetSearchInFilenames(v bool) {
	g.searchInFilenames = v
}

func (g *Github) SetTimeout(d time.Duration) {
	g.timeout = d
}

func (g *Github) Search(terms string, opts ...Opt) (Results, error) {
	searchstring := terms
	if g.org != "" {
		searchstring = "org:" + g.org + " " + terms
	}
	for _, opt := range opts {
		opt(g)
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
	ctx, cancel := searchContext(g.timeout)
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
		sopts.Page = response.NextPage
	}
	return g.toResult(ctx, client, csresults)
}

const maxAttempts = 3

// retry runs fn until it succeeds, until it fails with something other than a
// rate limit, or until maxAttempts have been made. Pagination must not be
// driven from here: mixing the two is what previously let a permanently rate
// limited search spin forever without ever returning an error.
func retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; ; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err = fn(); err == nil {
			return nil
		}
		delay, retryable := rateLimitDelay(err)
		if !retryable {
			return err
		}
		if attempt >= maxAttempts {
			return fmt.Errorf("giving up after %d attempts: %w", maxAttempts, err)
		}
		logrus.Debugf("Hit rate limit, waiting %s before retry %d/%d", delay, attempt+1, maxAttempts)
		if serr := sleep(ctx, delay); serr != nil {
			return serr
		}
	}
}

// rateLimitDelay reports how long to wait before retrying err, and whether err
// is a rate limit at all. The reset time is in the past once the window has
// already rolled over, which must not turn into a negative sleep.
func rateLimitDelay(err error) (time.Duration, bool) {
	var rlErr *github.RateLimitError
	if errors.As(err, &rlErr) {
		return max(time.Until(rlErr.Rate.Reset.Time), 0), true
	}
	var abuseErr *github.AbuseRateLimitError
	if errors.As(err, &abuseErr) {
		if abuseErr.RetryAfter == nil {
			return 0, true
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

func (g *Github) toResult(ctx context.Context, client *github.Client, csresults []*github.CodeResult) (Results, error) {
	var results Results
	for _, res := range csresults {
		logrus.Debugf("Result:\n")
		logrus.Debugf("  Name: %s:\n", res.GetName())
		logrus.Debugf("  Path: %s:\n", res.GetPath())
		logrus.Debugf("  SHA: %s:\n", res.GetSHA())
		logrus.Debugf("  HTMLURL: %s:\n", res.GetHTMLURL())
		logrus.Debugf("  Repository: %+v:\n", res.Repository)
		logrus.Debugf("  TextMatches:\n")
		for idx, tm := range res.TextMatches {
			fullPath := fmt.Sprintf("%s/%s/%s", res.GetRepository().GetOwner().GetLogin(), res.GetRepository().GetName(), res.GetPath())
			var rc *github.RepositoryContent
			err := retry(ctx, func() error {
				logrus.Debugf("Fetching file content, owner=%q repo=%q path=%q",
					res.GetRepository().GetOwner().GetLogin(),
					res.GetRepository().GetName(),
					res.GetPath(),
				)
				var (
					resp *github.Response
					err  error
				)
				rc, _, resp, err = client.Repositories.GetContents(
					ctx,
					res.GetRepository().GetOwner().GetLogin(),
					res.GetRepository().GetName(),
					res.GetPath(),
					&github.RepositoryContentGetOptions{},
				)
				logrus.Debugf("Response: %+v", resp)
				logrus.Debugf("RepositoryContent: %+v", rc)
				return err
			})
			if err != nil {
				return nil, fmt.Errorf("failed to get content of file %q: %w", fullPath, err)
			}
			// GetContents returns a nil file content without an error when the
			// path turns out to be a directory.
			if rc == nil {
				return nil, fmt.Errorf("no file content returned for %q", fullPath)
			}
			// GetContent decodes according to the reported encoding and
			// reports the >1MB "none" case, rather than dereferencing a
			// Content that GitHub may not have populated at all.
			fullText, err := rc.GetContent()
			if err != nil {
				return nil, fmt.Errorf("failed to read content of file %q: %w", fullPath, err)
			}
			// find fragment in full text
			fragmentStart := strings.Index(fullText, tm.GetFragment())
			if fragmentStart == -1 {
				return nil, fmt.Errorf("code fragment not found in full file content")
			}

			lines := strings.Split(fullText, "\n")
			logrus.Debugf("    %d) text match:\n", idx+1)
			logrus.Debugf("        ObjectURL: %s\n", tm.GetObjectURL())
			logrus.Debugf("        ObjectType: %s\n", tm.GetObjectType())
			logrus.Debugf("        Property: %s\n", tm.GetProperty())
			logrus.Debugf("        Fragment: %s\n", tm.GetFragment())
			logrus.Debugf("        Matches:\n")
			for _, match := range tm.Matches {
				// indices is documented as a start/end pair but is omitted
				// entirely for some match kinds
				if match == nil || len(match.Indices) < 2 {
					logrus.Debugf("Skipping match with no indices")
					continue
				}
				// start of the highlight, relative to the full file content
				start := fragmentStart + match.Indices[0]
				length := match.Indices[1] - match.Indices[0]
				lineno := strings.Count((fullText)[:start], "\n") + 1
				// start of the highlight, relative to the line rather than to
				// the full text
				startInLine := start
				for idx, line := range lines {
					if idx+1 == lineno {
						break
					}
					startInLine -= len(line) + 1
				}
				line := lines[lineno-1]
				// try adding line number
				fileURLwithLineno, err := url.Parse(res.GetHTMLURL())
				if err != nil {
					return nil, fmt.Errorf("invalid file URL %q: %q", res.GetHTMLURL(), err)
				}
				fileURLwithLineno.Fragment = fmt.Sprintf("L%d", lineno+1)
				beforeIdx := lineno - 1 - g.linesBefore
				if beforeIdx < 0 {
					beforeIdx = 0
				}
				afterIdx := lineno + g.linesAfter
				if afterIdx > len(lines) {
					afterIdx = len(lines)
				}
				result := Result{
					Backend: g.Name(),
					Line:    line,
					Lineno:  lineno,
					Context: ResultContext{
						Before: lines[beforeIdx : lineno-1],
						After:  lines[lineno:afterIdx],
					},
					Highlight: [2]int{startInLine, startInLine + length},
					Path:      res.GetPath(),
					RepoURL:   res.GetRepository().GetHTMLURL(),
					FileURL:   fileURLwithLineno.String(),
					Owner:     res.GetRepository().GetOwner().GetLogin(),
					RepoName:  res.GetRepository().GetName(),
				}
				results = append(results, result)
			}
		}
	}
	return results, nil
}
