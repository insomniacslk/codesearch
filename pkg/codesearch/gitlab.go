package codesearch

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// Gitlab implements the Backend interface
type Gitlab struct {
	name        string
	apiEndpoint string
	token       string
	group       string
	project     string
}

func (g *Gitlab) New(name string, params BackendParams) (Backend, error) {
	group, err := gitlabScopeParam(params, "group")
	if err != nil {
		return nil, err
	}
	project, err := gitlabScopeParam(params, "project")
	if err != nil {
		return nil, err
	}
	if group != "" && project != "" {
		return nil, fmt.Errorf("cannot specify both 'project' and 'group'")
	}
	token, err := params.ResolveToken()
	if err != nil {
		return nil, err
	}
	apiEndpoint := params.GetString("api_endpoint")
	if apiEndpoint == nil {
		return nil, fmt.Errorf("missing 'api_endpoint' parameter")
	}
	return &Gitlab{
		name:        name,
		token:       token,
		apiEndpoint: *apiEndpoint,
		group:       group,
		project:     project,
	}, nil
}

// gitlabScopeParam reads a group or project, which may be a numeric ID or a
// full path. YAML decodes a bare number as an int or a float, and GetString
// would drop it, silently widening the search to every project the token can
// see; so numbers are accepted and any other type is an error.
func gitlabScopeParam(params BackendParams, key string) (string, error) {
	switch v := params.Get(key).(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprint(v), nil
	case float64:
		if v == math.Trunc(v) && !math.IsInf(v, 0) {
			return strconv.FormatFloat(v, 'f', -1, 64), nil
		}
		return "", fmt.Errorf("parameter '%s' must be an integer ID or a path, got %v", key, v)
	default:
		return "", fmt.Errorf("parameter '%s' must be an integer ID or a path, got %T", key, v)
	}
}

func (g *Gitlab) Name() string {
	return g.name
}

func (g *Gitlab) Group() string {
	return g.group
}

func (g *Gitlab) Project() string {
	return g.project
}

func (g *Gitlab) Type() string {
	return BackendTypeGitlab
}

// Supports implements Backend. GitLab's advanced search offers no
// case-sensitivity switch, and the backend does not implement a
// file-name-only mode; file name matches surface on their own as part of a
// normal search and are reported with Result.IsFilename set.
func (g *Gitlab) Supports(Capability) bool {
	return false
}

func (g *Gitlab) Search(ctx context.Context, searchString string, opts SearchOptions) (Results, error) {
	u, err := url.Parse(g.apiEndpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Gitlab API endpoint: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	if u.Host == "" {
		u.Host = "gitlab.com"
	}
	if u.Path == "" {
		u.Path = "/api/v4"
	}
	client, err := gitlab.NewClient(g.token, gitlab.WithBaseURL(u.String()))
	if err != nil {
		return nil, fmt.Errorf("failed to set up Gitlab client: %w", err)
	}
	ctx, cancel := searchContext(ctx, opts.Timeout)
	defer cancel()
	withCtx := gitlab.WithContext(ctx)
	// The search endpoints take an ID or a URL-encoded path, so the configured
	// group or project is passed straight through. Resolving it to a numeric
	// ID first would mean listing every group or project the token can see.
	var (
		scope  string
		search func(*gitlab.SearchOptions) ([]*gitlab.Blob, *gitlab.Response, error)
	)
	switch {
	case g.group != "":
		scope = fmt.Sprintf("group %q", g.group)
		search = func(sopts *gitlab.SearchOptions) ([]*gitlab.Blob, *gitlab.Response, error) {
			return client.Search.BlobsByGroup(g.group, searchString, sopts, withCtx)
		}
	case g.project != "":
		scope = fmt.Sprintf("project %q", g.project)
		search = func(sopts *gitlab.SearchOptions) ([]*gitlab.Blob, *gitlab.Response, error) {
			return client.Search.BlobsByProject(g.project, searchString, sopts, withCtx)
		}
	default:
		scope = "all projects"
		search = func(sopts *gitlab.SearchOptions) ([]*gitlab.Blob, *gitlab.Response, error) {
			return client.Search.Blobs(searchString, sopts, withCtx)
		}
	}

	var blobs []*gitlab.Blob
	sopts := gitlab.SearchOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	for {
		someBlobs, response, err := search(&sopts)
		logrus.Debugf("Search response: %+v", response)
		if err != nil {
			return nil, fmt.Errorf("failed to search blobs in %s: %w", scope, err)
		}
		blobs = append(blobs, someBlobs...)
		if response.NextPage == 0 {
			break
		}
		if opts.Limit > 0 && uint(len(blobs)) >= opts.Limit {
			logrus.Debugf("Stopping after %d blobs, enough for a limit of %d", len(blobs), opts.Limit)
			break
		}
		sopts.Page = response.NextPage
	}
	return g.toResult(client, withCtx, searchString, blobs, opts)
}

func (g *Gitlab) toResult(client *gitlab.Client, withCtx gitlab.RequestOptionFunc, searchString string, blobs []*gitlab.Blob, opts SearchOptions) (Results, error) {
	var (
		results  Results
		projects = make(map[int64]*gitlab.Project)
	)
	for _, blob := range blobs {
		logrus.Debugf("Result:")
		logrus.Debugf("  Basename: %s:", blob.Basename)
		logrus.Debugf("  Data: %s:", blob.Data)
		logrus.Debugf("  Path: %s:", blob.Path)
		logrus.Debugf("  Filename: %s:", blob.Filename)
		logrus.Debugf("  ID: %s", blob.ID)
		logrus.Debugf("  Ref: %s", blob.Ref)
		logrus.Debugf("  Startline: %d", blob.Startline)
		logrus.Debugf("  ProjectID: %d", blob.ProjectID)

		if _, ok := projects[blob.ProjectID]; !ok {
			project, response, err := client.Projects.GetProject(blob.ProjectID, &gitlab.GetProjectOptions{}, withCtx)
			logrus.Debugf("Projects.GetProject response: %+v", response)
			if err != nil {
				return nil, fmt.Errorf("failed to get project with ID %d: %w", blob.ProjectID, err)
			}
			projects[blob.ProjectID] = project
		}
		logrus.Debugf("  Project Name: %s", projects[blob.ProjectID].Name)

		project := projects[blob.ProjectID]
		// a project without a namespace should not happen, but the field is
		// a pointer and a crash would lose every other result
		var owner string
		if project.Namespace != nil {
			owner = project.Namespace.Path
		}
		result := Result{
			Backend:  g.Name(),
			Path:     blob.Path,
			RepoURL:  project.WebURL,
			FileURL:  fmt.Sprintf("%s/-/blob/%s/%s", project.WebURL, project.DefaultBranch, blob.Path),
			Owner:    owner,
			RepoName: project.Path,
			Branch:   project.DefaultBranch,
		}
		lines := strings.Split(blob.Data, "\n")
		idx, highlight, ok := gitlabLocate(lines, searchString)
		if blob.Data == "" || (!ok && gitlabMatchesPath(blob.Path, searchString)) {
			// The search pattern was found in the file name, not in the file
			// content, so it's marked as such. Line, StartLine, Lineno, Context
			// and Highlight are not set
			result.IsFilename = true
		} else {
			// When nothing can be located (e.g. advanced search syntax), GitLab
			// still returned content that matched, so the snippet's first line
			// is reported without a highlight rather than dropped.
			// TODO fetch entire file content if the requested context is longer
			// than the available one
			lineno := int(blob.Startline) + idx
			result.Line = lines[idx]
			result.Lineno = lineno
			result.Highlight = highlight
			result.Context.Before, result.Context.After = contextLines(lines, idx+1, opts.LinesBefore, opts.LinesAfter)
			// add line fragment to URL
			result.FileURL = fmt.Sprintf("%s/-/blob/%s/%s#L%d", project.WebURL, project.DefaultBranch, blob.Path, lineno)
		}
		results = append(results, result)
	}
	return results, nil
}

// gitlabLocate finds the line a blob matched on and the match within it. The
// whole query is tried first; failing that, its individual terms, because
// GitLab's advanced search tokenises the query and a blob can match without
// containing it verbatim. Offsets come from a case-insensitive regexp on the
// original text, as lowercasing can change byte lengths (e.g. "İ").
func gitlabLocate(lines []string, searchString string) (int, [2]int, bool) {
	for _, patterns := range [][]string{{searchString}, strings.Fields(searchString)} {
		var res []*regexp.Regexp
		for _, p := range patterns {
			res = append(res, regexp.MustCompile("(?i)"+regexp.QuoteMeta(p)))
		}
		for i, line := range lines {
			for _, re := range res {
				if loc := re.FindStringIndex(line); loc != nil {
					return i, [2]int{loc[0], loc[1]}, true
				}
			}
		}
	}
	return 0, [2]int{}, false
}

// gitlabMatchesPath reports whether any term of the query occurs in the path.
func gitlabMatchesPath(path, searchString string) bool {
	path = strings.ToLower(path)
	for _, term := range strings.Fields(searchString) {
		if strings.Contains(path, strings.ToLower(term)) {
			return true
		}
	}
	return false
}
