package codesearch

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/sirupsen/logrus"
	gitlab "github.com/xanzy/go-gitlab"
)

// Gitlab implements the Backend interface
type Gitlab struct {
	name              string
	apiEndpoint       string
	token             string
	group             string
	project           string
	linesBefore       int
	linesAfter        int
	caseInsensitive   bool
	searchInFilenames bool
}

func (g *Gitlab) New(name string, params BackendParams) (Backend, error) {
	group := params.GetString("group")
	project := params.GetString("project")
	if group != nil && project != nil {
		return nil, fmt.Errorf("cannot specify both 'project' and 'group'")
	}
	token := params.GetString("token")
	if token == nil {
		return nil, fmt.Errorf("missing 'token' parameter")
	}
	apiEndpoint := params.GetString("api_endpoint")
	if apiEndpoint == nil {
		return nil, fmt.Errorf("missing 'apiEndpoint' parameter")
	}
	gl := Gitlab{
		name:        name,
		token:       *token,
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

func (g *Gitlab) SetCaseInsensitive(v bool) {
	g.caseInsensitive = v
}

func (g *Gitlab) SetLinesBefore(n int) {
	g.linesBefore = n
}

func (g *Gitlab) SetLinesAfter(n int) {
	g.linesAfter = n
}

func (g *Gitlab) SetSearchInFilenames(v bool) {
	g.searchInFilenames = v
}

func (g *Gitlab) Search(searchString string, opts ...Opt) (Results, error) {
	for _, opt := range opts {
		opt(g)
	}
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
	var (
		blobs []*gitlab.Blob
	)
	if g.group != "" {
		// get group ID
		// XXX Should this request be paginated as well?
		groups, response, err := client.Groups.ListGroups(&gitlab.ListGroupsOptions{})
		logrus.Debugf("Search.ListGroups response: %+v", response)
		if err != nil {
			return nil, fmt.Errorf("failed to get group list: %w", err)
		}
		groupID := -1
		for _, group := range groups {
			if group.Name == g.group {
				groupID = group.ID
				break
			}
		}
		if groupID == -1 {
			return nil, fmt.Errorf("group %q not found", g.group)
		}
		sopts := gitlab.SearchOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
		for {
			someBlobs, response, err := client.Search.BlobsByGroup(groupID, searchString, &sopts)
			logrus.Debugf("Search.BlobsByGroup response: %+v", response)
			if err != nil {
				return nil, fmt.Errorf("failed to search blobs by project: %w", err)
			}
			sopts.Page = response.NextPage
			blobs = append(blobs, someBlobs...)
			if response.NextPage == 0 {
				break
			}
		}
	} else if g.project != "" {
		// get project ID
		// XXX Should this request be paginated as well?
		projects, response, err := client.Projects.ListProjects(&gitlab.ListProjectsOptions{})
		logrus.Debugf("Search.ListProjects response: %+v", response)
		if err != nil {
			return nil, fmt.Errorf("failed to get project list: %w", err)
		}
		projectID := 0
		for _, proj := range projects {
			if proj.Name == g.project {
				projectID = proj.ID
				break
			}
		}
		sopts := gitlab.SearchOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
		for {
			someBlobs, response, err := client.Search.BlobsByProject(projectID, searchString, &sopts)
			logrus.Debugf("Search.BlobsByProject response: %+v", response)
			if err != nil {
				return nil, fmt.Errorf("failed to search blobs by group: %w", err)
			}
			sopts.Page = response.NextPage
			blobs = append(blobs, someBlobs...)
			if response.NextPage == 0 {
				break
			}
		}
	} else {
		sopts := gitlab.SearchOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
		for {
			someBlobs, response, err := client.Search.Blobs(searchString, &sopts)
			logrus.Debugf("Search.Blobs response: %+v", response)
			if err != nil {
				return nil, fmt.Errorf("failed to search blobs: %w", err)
			}
			sopts.Page = response.NextPage
			blobs = append(blobs, someBlobs...)
			if response.NextPage == 0 {
				break
			}
		}
	}
	return g.toResult(client, searchString, blobs)
}

func (g *Gitlab) toResult(client *gitlab.Client, searchString string, blobs []*gitlab.Blob) (Results, error) {
	var (
		results  Results
		projects = make(map[int]*gitlab.Project, 0)
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
			project, response, err := client.Projects.GetProject(blob.ProjectID, &gitlab.GetProjectOptions{})
			logrus.Debugf("Projects.GetProject response: %+v", response)
			if err != nil {
				return nil, fmt.Errorf("failed to get project with ID %q: %w", blob.ProjectID, err)
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
			beforeIdx := linenoInBlob - g.linesBefore
			if beforeIdx < 0 {
				beforeIdx = 0
			}
			afterIdx := linenoInBlob + g.linesAfter + 1
			if afterIdx > len(lines) {
				afterIdx = len(lines)
			}
			result.Lineno = blob.Startline
			result.Context = ResultContext{
				Before: lines[beforeIdx:linenoInBlob],
				After:  lines[linenoInBlob+1 : afterIdx],
			}
			result.Highlight = [2]int{start, end}
			result.Line = line
			result.Lineno = blob.Startline + linenoInBlob
			// add line fragment to URL
			result.FileURL = fmt.Sprintf("%s/-/blob/%s/%s#L%d", project.WebURL, project.DefaultBranch, blob.Path, blob.Startline+linenoInBlob)
		}
		results = append(results, result)
	}
	return results, nil
}
