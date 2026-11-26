package codesearch

import (
	"context"
	"fmt"
	"net/url"
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
	group := params.GetString("group")
	project := params.GetString("project")
	if group != nil && project != nil {
		return nil, fmt.Errorf("cannot specify both 'project' and 'group'")
	}
	token, err := params.ResolveToken()
	if err != nil {
		return nil, err
	}
	apiEndpoint := params.GetString("api_endpoint")
	if apiEndpoint == nil {
		return nil, fmt.Errorf("missing 'apiEndpoint' parameter")
	}
	gl := Gitlab{
		name:        name,
		token:       token,
		apiEndpoint: *apiEndpoint,
	}
	if group != nil {
		gl.group = *group
	}
	if project != nil {
		gl.project = *project
	}
	return &gl, nil
}

func (g *Gitlab) Name() string {
	return g.name
}

func (g *Gitlab) Group() string {
	return g.group
}

func (g *Gitlab) Projet() string {
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
		startOffset := strings.Index(strings.ToLower(blob.Data), strings.ToLower(searchString))
		var (
			start, end int
			line       string
		)
		result := Result{
			Backend:    g.Name(),
			IsFilename: false,
			Path:       blob.Path,
			RepoURL:    project.WebURL,
			FileURL:    fmt.Sprintf("%s/-/blob/%s/%s", project.WebURL, project.DefaultBranch, blob.Path),
			Owner:      project.Namespace.Path,
			RepoName:   project.Path,
			Branch:     project.DefaultBranch,
		}
		if startOffset == -1 {
			// The search pattern was found in the file name, not in the file
			// content, so it's marked as such. Line, StartLine, Lineno, Context
			// and Highlight are not set
			result.IsFilename = true
		} else {
			lines := strings.Split(blob.Data, "\n")
			linenoInBlob := strings.Count(blob.Data[:startOffset], "\n")
			line = lines[linenoInBlob]
			start = strings.Index(strings.ToLower(line), strings.ToLower(searchString))
			end = start + len(searchString)
			// TODO fetch entire file content if the requested context is longer
			// than the available one
			result.Context.Before, result.Context.After = contextLines(lines, linenoInBlob+1, opts.LinesBefore, opts.LinesAfter)
			result.Highlight = [2]int{start, end}
			result.Line = line
			result.Lineno = int(blob.Startline) + linenoInBlob
			// add line fragment to URL
			result.FileURL = fmt.Sprintf("%s/-/blob/%s/%s#L%d", project.WebURL, project.DefaultBranch, blob.Path, int(blob.Startline)+linenoInBlob)
		}
		results = append(results, result)
	}
	return results, nil
}
