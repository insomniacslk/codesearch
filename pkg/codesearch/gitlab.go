package codesearch

import (
	"fmt"
	"net/url"

	"github.com/sirupsen/logrus"
	gitlab "github.com/xanzy/go-gitlab"
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

func (g *Gitlab) Search(terms string, opts ...Opt) (Results, error) {
	searchstring := terms
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
		blobs    []*gitlab.Blob
		response *gitlab.Response
	)
	if g.group != "" {
		// get group ID
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
		blobs, response, err = client.Search.BlobsByGroup(groupID, searchstring, &gitlab.SearchOptions{})
		logrus.Debugf("Search.BlobsByGroup response: %+v", response)
		if err != nil {
			return nil, fmt.Errorf("failed to search blobs by project: %w", err)
		}
	} else if g.project != "" {
		// get project ID
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
		blobs, response, err = client.Search.BlobsByProject(projectID, searchstring, &gitlab.SearchOptions{})
		logrus.Debugf("Search.BlobsByProject response: %+v", response)
		if err != nil {
			return nil, fmt.Errorf("failed to search blobs by group: %w", err)
		}
	} else {
		blobs, response, err = client.Search.Blobs(searchstring, &gitlab.SearchOptions{})
		logrus.Debugf("Search.Blobs response: %+v", response)
		if err != nil {
			return nil, fmt.Errorf("failed to search blobs: %w", err)
		}
	}
	return g.toResult(client, blobs)
}

func (g *Gitlab) toResult(client *gitlab.Client, blobs []*gitlab.Blob) (Results, error) {
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
		logrus.Debugf("  ID: %s:", blob.ID)
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
		logrus.Debugf("  Project Name: %s", projects[blob.ProjectID])

		project := projects[blob.ProjectID]
		results = append(results, Result{
			Backend:   g.Name(),
			Line:      blob.Data,
			Lineno:    blob.Startline,
			Context:   ResultContext{},
			Highlight: [2]int{0, 0},
			Path:      blob.Path,
			RepoURL:   project.WebURL,
			FileURL:   fmt.Sprintf("%s/-/blob/%s/%s", project.WebURL, project.DefaultBranch, blob.Path),
			Owner:     project.Namespace.Path,
			RepoName:  project.Path,
			Branch:    project.DefaultBranch,
		})
	}
	return results, nil
}
