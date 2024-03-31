package codesearch

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-github/v60/github"
	"github.com/sirupsen/logrus"
)

// Github implements the Backend interface
type Github struct {
	name        string
	apiEndpoint string
	token       string
	org         string
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
	ctx := context.Background()
	sopts := github.SearchOptions{TextMatch: true}
	var csresults []*github.CodeResult
loop:
	for {
		for attempt := 0; attempt < 3; attempt++ {
			results, response, err := client.Search.Code(ctx, searchstring, &sopts)
			logrus.Debugf("Response: %+v", response)
			if err != nil {
				if rlerr, ok := err.(*github.RateLimitError); ok {
					delay := time.Until(rlerr.Rate.Reset.Time)
					logrus.Debugf("Hit rate limit, waiting %s before retrying", delay)
					time.Sleep(delay)
					continue
				}
				return nil, fmt.Errorf("search failed: %w", err)
			}
			csresults = append(csresults, results.CodeResults...)
			if response.NextPage == 0 {
				break loop
			}
			sopts.Page = response.NextPage
		}
	}
	return g.toResult(ctx, client, csresults)
}

func (g *Github) toResult(ctx context.Context, client *github.Client, csresults []*github.CodeResult) (Results, error) {
	var results Results
	for _, res := range csresults {
		logrus.Debugf("Result:\n")
		logrus.Debugf("  Name: %s:\n", *res.Name)
		logrus.Debugf("  Path: %s:\n", *res.Path)
		logrus.Debugf("  SHA: %s:\n", *res.SHA)
		logrus.Debugf("  HTMLURL: %s:\n", *res.HTMLURL)
		logrus.Debugf("  Repository: %+v:\n", res.Repository)
		logrus.Debugf("  TextMatches:\n")
		for idx, tm := range res.TextMatches {
			fullPath := fmt.Sprintf("%s/%s/%s", *res.Repository.Owner.Login, *res.Repository.Name, *res.Path)
			var (
				rc   *github.RepositoryContent
				resp *github.Response
				err  error
			)
			for attempt := 0; attempt < 3; attempt++ {
				logrus.Debugf("Fetching file content, owner=%q repo=%q path=%q",
					*res.Repository.Owner.Login,
					*res.Repository.Name,
					*res.Path,
				)
				rc, _, resp, err = client.Repositories.GetContents(
					ctx,
					*res.Repository.Owner.Login,
					*res.Repository.Name,
					*res.Path,
					&github.RepositoryContentGetOptions{},
				)
				logrus.Debugf("Response: %+v", resp)
				logrus.Debugf("RepositoryContent: %+v", rc)
				if err != nil {
					if rlerr, ok := err.(*github.RateLimitError); ok {
						delay := time.Until(rlerr.Rate.Reset.Time)
						logrus.Debugf("Hit rate limit, waiting %s before retrying", delay)
						time.Sleep(delay)
						continue
					}
					return nil, fmt.Errorf("failed to get content of file %q: %w", fullPath, err)
				}
				break
			}
			b64bytes, err := base64.StdEncoding.DecodeString(*rc.Content)
			if err != nil {
				return nil, fmt.Errorf("failed to base64-decode content of file %q: %w", fullPath, err)
			}
			fullText := string(b64bytes)
			// find fragment in full text
			fragmentStart := strings.Index(fullText, *tm.Fragment)
			if fragmentStart == -1 {
				return nil, fmt.Errorf("code fragment not found in full file content")
			}

			lines := strings.Split(fullText, "\n")
			logrus.Debugf("    %d) text match:\n", idx+1)
			logrus.Debugf("        ObjectURL: %s\n", *tm.ObjectURL)
			logrus.Debugf("        ObjectType: %s\n", *tm.ObjectType)
			logrus.Debugf("        Property: %s\n", *tm.Property)
			logrus.Debugf("        Fragment: %s\n", *tm.Fragment)
			logrus.Debugf("        Matches:\n")
			for _, match := range tm.Matches {
				// start of the highlight, relative to the full file content
				start := fragmentStart + match.Indices[0]
				length := fragmentStart + match.Indices[1] - start
				lineno := strings.Count((fullText)[:start], "\n")
				// start of the highlight, relative to the line rather than to
				// the full text
				startInLine := start
				for idx, line := range lines {
					if idx == lineno {
						break
					}
					startInLine -= len(line) + 1
				}
				line := lines[lineno]
				// try adding line number
				fileURLwithLineno, err := url.Parse(*res.HTMLURL)
				if err != nil {
					return nil, fmt.Errorf("invalid file URL %q: %q", *res.HTMLURL, err)
				}
				fileURLwithLineno.Fragment = fmt.Sprintf("L%d", lineno+1)
				result := Result{
					Backend: g.Name(),
					Line:    line,
					Lineno:  lineno + 1,
					Context: ResultContext{
						Before: nil,
						After:  nil,
					},
					Highlight: [2]int{startInLine, startInLine + length},
					Path:      *res.Path,
					RepoURL:   *res.Repository.HTMLURL,
					FileURL:   fileURLwithLineno.String(),
					Owner:     *res.Repository.Owner.Login,
					RepoName:  *res.Repository.Name,
				}
				results = append(results, result)
			}
		}
	}
	return results, nil
}
