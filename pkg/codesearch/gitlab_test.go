package codesearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// gitlabRecorder serves a canned GitLab API and records the paths it was
// asked for, so a test can assert which endpoints were reached.
type gitlabRecorder struct {
	mu      sync.Mutex
	paths   []string
	handler func(w http.ResponseWriter, r *http.Request) bool
}

func (g *gitlabRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.paths = append(g.paths, r.URL.EscapedPath())
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if g.handler != nil && g.handler(w, r) {
		return
	}
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"message":"404 Not Found"}`)
}

func (g *gitlabRecorder) requested(substr string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.paths {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

func newGitlabBackend(t *testing.T, url string, params BackendParams) *Gitlab {
	t.Helper()
	params["token"] = "testtoken"
	params["api_endpoint"] = url + "/api/v4"
	b, err := (&Gitlab{}).New("test", params)
	if err != nil {
		t.Fatalf("failed to build backend: %v", err)
	}
	return b.(*Gitlab)
}

// TestGitlabAddressesScopeDirectly checks that the group or project is passed
// to the search endpoint as a URL-encoded path, without first listing every
// group or project to resolve a numeric ID.
func TestGitlabAddressesScopeDirectly(t *testing.T) {
	for _, tt := range []struct {
		name       string
		params     BackendParams
		wantPath   string
		wantNoList string
	}{
		{
			name:       "group",
			params:     BackendParams{"group": "mygroup"},
			wantPath:   "/api/v4/groups/mygroup/-/search",
			wantNoList: "/api/v4/groups?",
		},
		{
			name:       "nested group",
			params:     BackendParams{"group": "top/nested"},
			wantPath:   "/api/v4/groups/top%2Fnested/-/search",
			wantNoList: "/api/v4/groups?",
		},
		{
			name:       "project",
			params:     BackendParams{"project": "myorg/myproject"},
			wantPath:   "/api/v4/projects/myorg%2Fmyproject/-/search",
			wantNoList: "/api/v4/projects?",
		},
		{
			name:     "no scope",
			params:   BackendParams{},
			wantPath: "/api/v4/search",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := &gitlabRecorder{handler: func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "/search") {
					fmt.Fprint(w, `[]`)
					return true
				}
				return false
			}}
			srv := httptest.NewServer(rec)
			defer srv.Close()

			if _, err := newGitlabBackend(t, srv.URL, tt.params).Search(context.Background(), "needle", SearchOptions{}); err != nil {
				t.Fatalf("search failed: %v", err)
			}
			if !rec.requested(tt.wantPath) {
				t.Errorf("did not request %q, got %v", tt.wantPath, rec.paths)
			}
			if tt.wantNoList != "" && rec.requested(tt.wantNoList) {
				t.Errorf("listed every group/project instead of addressing the scope directly")
			}
		})
	}
}

// TestGitlabScopeBeyondFirstPage is the regression test for the pagination
// bug: the old code resolved the scope by listing groups or projects without
// paginating, so anything past the first page reported "not found".
func TestGitlabScopeBeyondFirstPage(t *testing.T) {
	for _, tt := range []struct {
		name, listPath, searchPath string
		params                     BackendParams
	}{
		{
			name:       "group",
			listPath:   "/api/v4/groups",
			searchPath: "/api/v4/groups/wanted/-/search",
			params:     BackendParams{"group": "wanted"},
		},
		{
			name:       "project",
			listPath:   "/api/v4/projects",
			searchPath: "/api/v4/projects/wanted/-/search",
			params:     BackendParams{"project": "wanted"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := &gitlabRecorder{handler: func(w http.ResponseWriter, r *http.Request) bool {
				switch r.URL.Path {
				case tt.listPath:
					// a first page that does not contain the wanted scope
					fmt.Fprint(w, `[{"id":1,"name":"unrelated","path":"unrelated"}]`)
					return true
				case tt.searchPath:
					fmt.Fprint(w, `[{"basename":"f","data":"needle\n","path":"f.go",
						"filename":"f.go","id":null,"ref":"main","startline":1,"project_id":9}]`)
					return true
				case "/api/v4/projects/9":
					fmt.Fprint(w, `{"id":9,"name":"proj","path":"proj","default_branch":"main",
						"web_url":"https://gitlab.example.com/ns/proj","namespace":{"path":"ns"}}`)
					return true
				}
				return false
			}}
			srv := httptest.NewServer(rec)
			defer srv.Close()

			results, err := newGitlabBackend(t, srv.URL, tt.params).Search(context.Background(), "needle", SearchOptions{})
			if err != nil {
				t.Fatalf("search failed: %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("got %d results, want 1", len(results))
			}
		})
	}
}

// TestGitlabUnknownScope checks that a scope the server rejects surfaces as an
// error naming it, rather than silently searching something else.
func TestGitlabUnknownScope(t *testing.T) {
	rec := &gitlabRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	_, err := newGitlabBackend(t, srv.URL, BackendParams{"project": "missing"}).Search(context.Background(), "needle", SearchOptions{})
	if err == nil {
		t.Fatal("got nil error, want a failure for an unknown project")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("got error %q, want it to name the project", err)
	}
	if rec.requested("/projects/0/") {
		t.Error("searched project 0 instead of failing")
	}
}

func TestGitlabPaginatesBlobs(t *testing.T) {
	var pages int
	rec := &gitlabRecorder{handler: func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/search") {
			if r.URL.Path == "/api/v4/projects/9" {
				fmt.Fprint(w, `{"id":9,"name":"proj","path":"proj","default_branch":"main",
					"web_url":"https://gitlab.example.com/ns/proj","namespace":{"path":"ns"}}`)
				return true
			}
			return false
		}
		pages++
		if pages < 3 {
			w.Header().Set("X-Next-Page", fmt.Sprint(pages+1))
		}
		fmt.Fprintf(w, `[{"basename":"f","data":"needle\n","path":"f%d.go",
			"filename":"f%d.go","ref":"main","startline":1,"project_id":9}]`, pages, pages)
		return true
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	results, err := newGitlabBackend(t, srv.URL, BackendParams{}).Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if got, want := len(results), 3; got != want {
		t.Errorf("got %d results across %d pages, want %d", got, pages, want)
	}
}

func TestGitlabRejectsGroupAndProjectTogether(t *testing.T) {
	_, err := (&Gitlab{}).New("test", BackendParams{
		"token":        "t",
		"api_endpoint": "https://gitlab.example.com/api/v4",
		"group":        "g",
		"project":      "p",
	})
	if err == nil {
		t.Fatal("got nil error, want a failure for group and project together")
	}
}

// gitlabBlobSearch serves a single blob with the given data and path from
// project 9 and runs a search against it.
func gitlabBlobSearch(t *testing.T, query, data, path string, startline int, opts SearchOptions) Results {
	t.Helper()
	blob, err := json.Marshal(map[string]interface{}{
		"basename": path, "data": data, "path": path, "filename": path,
		"ref": "main", "startline": startline, "project_id": 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &gitlabRecorder{handler: func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = fmt.Fprintf(w, `[%s]`, blob)
			return true
		case r.URL.Path == "/api/v4/projects/9":
			_, _ = fmt.Fprint(w, `{"id":9,"name":"proj","path":"proj","default_branch":"main",
				"web_url":"https://gitlab.example.com/ns/proj","namespace":{"path":"ns"}}`)
			return true
		}
		return false
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	results, err := newGitlabBackend(t, srv.URL, BackendParams{}).Search(context.Background(), query, opts)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	return results
}

// TestGitlabNegativeContext is the regression test for a panic: a negative
// LinesBefore or LinesAfter produced inverted slice bounds.
func TestGitlabNegativeContext(t *testing.T) {
	for _, opts := range []SearchOptions{{LinesBefore: -1}, {LinesAfter: -2}, {LinesBefore: -5, LinesAfter: -5}} {
		res := gitlabBlobSearch(t, "needle", "a\nneedle\nb\n", "f.go", 1, opts)[0]
		if res.Line != "needle" || res.Lineno != 2 {
			t.Errorf("%+v: got line %q at %d, want \"needle\" at 2", opts, res.Line, res.Lineno)
		}
		if len(res.Context.Before) != 0 || len(res.Context.After) != 0 {
			t.Errorf("%+v: got context %+v, want none", opts, res.Context)
		}
	}
}
