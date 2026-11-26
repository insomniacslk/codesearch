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
	b, err := NewGitlab("test", params)
	if err != nil {
		t.Fatalf("failed to build backend: %v", err)
	}
	return b
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
	_, err := NewGitlab("test", BackendParams{
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

// TestGitlabMatchClassification is the regression test for multi-term
// queries: GitLab tokenises the query, so a blob can match without containing
// the whole string, and reporting it as a file name match made the CLI drop it.
func TestGitlabMatchClassification(t *testing.T) {
	for _, tt := range []struct {
		name, query, data, path string
		startline               int
		wantFilename            bool
		wantLine                string
		wantLineno              int
		wantHighlight           [2]int
	}{
		{
			name: "whole string", query: "Bar()", data: "func foo() {\n  bar()\n}\n", path: "f.go", startline: 10,
			wantLine: "  bar()", wantLineno: 11, wantHighlight: [2]int{2, 7},
		},
		{
			name: "terms", query: "bar foo", data: "func foo() {\n  bar()\n}\n", path: "f.go", startline: 10,
			wantLine: "func foo() {", wantLineno: 10, wantHighlight: [2]int{5, 8},
		},
		{
			name: "file name", query: "nothere Main", data: "package x\n", path: "cmd/main.go", startline: 1,
			wantFilename: true,
		},
		{
			name: "advanced syntax", query: "foo* -bar", data: "func food() {}\n", path: "f.go", startline: 3,
			wantLine: "func food() {}", wantLineno: 3,
		},
		{
			name: "empty data", query: "needle", data: "", path: "f.go", startline: 1,
			wantFilename: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := gitlabBlobSearch(t, tt.query, tt.data, tt.path, tt.startline, SearchOptions{})[0]
			if res.IsFilename != tt.wantFilename {
				t.Fatalf("got IsFilename %v, want %v", res.IsFilename, tt.wantFilename)
			}
			if tt.wantFilename {
				return
			}
			if res.Line != tt.wantLine || res.Lineno != tt.wantLineno || res.Highlight != tt.wantHighlight {
				t.Errorf("got line %q at %d highlight %v, want %q at %d highlight %v",
					res.Line, res.Lineno, res.Highlight, tt.wantLine, tt.wantLineno, tt.wantHighlight)
			}
		})
	}
}

// TestGitlabHighlightUnicode is the regression test for offsets computed on a
// lowercased copy: lowercasing "İ" adds a byte, which shifted the highlight
// past the match and out of the line.
func TestGitlabHighlightUnicode(t *testing.T) {
	res := gitlabBlobSearch(t, "needle", "İİ\nİİ needle\n", "f.go", 1, SearchOptions{})[0]
	if res.Line != "İİ needle" || res.Lineno != 2 {
		t.Fatalf("got line %q at %d, want \"İİ needle\" at 2", res.Line, res.Lineno)
	}
	if got := res.Line[res.Highlight[0]:res.Highlight[1]]; got != "needle" {
		t.Errorf("highlight %v covers %q, want \"needle\"", res.Highlight, got)
	}
}

// TestGitlabNumericScope is the regression test for a numeric group or
// project: YAML decodes it as a number, which was dropped, so the search ran
// over every project the token can see.
func TestGitlabNumericScope(t *testing.T) {
	for _, tt := range []struct {
		name               string
		params             BackendParams
		wantGroup, wantErr string
		wantProject        string
	}{
		{name: "int group", params: BackendParams{"group": 42}, wantGroup: "42"},
		{name: "int64 project", params: BackendParams{"project": int64(7)}, wantProject: "7"},
		{name: "uint group", params: BackendParams{"group": uint(42)}, wantGroup: "42"},
		{name: "integral float", params: BackendParams{"group": 42.0}, wantGroup: "42"},
		{name: "fractional float", params: BackendParams{"group": 42.5}, wantErr: "group"},
		{name: "bool", params: BackendParams{"project": true}, wantErr: "project"},
		{name: "empty group with project", params: BackendParams{"group": "", "project": "p"}, wantProject: "p"},
		{name: "nil group with project", params: BackendParams{"group": nil, "project": 3}, wantProject: "3"},
		{name: "both numeric", params: BackendParams{"group": 1, "project": 2}, wantErr: "both"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.params["token"] = "t"
			tt.params["api_endpoint"] = "https://gitlab.example.com/api/v4"
			gl, err := NewGitlab("test", tt.params)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got error %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewGitlab failed: %v", err)
			}
			if gl.Group() != tt.wantGroup || gl.Project() != tt.wantProject {
				t.Errorf("got group %q project %q, want %q and %q", gl.Group(), gl.Project(), tt.wantGroup, tt.wantProject)
			}
		})
	}
}

// TestGitlabNilNamespace checks that a project without a namespace does not
// crash result conversion.
func TestGitlabNilNamespace(t *testing.T) {
	rec := &gitlabRecorder{handler: func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = fmt.Fprint(w, `[{"basename":"f","data":"needle\n","path":"f.go","ref":"main","startline":1,"project_id":9}]`)
			return true
		case r.URL.Path == "/api/v4/projects/9":
			_, _ = fmt.Fprint(w, `{"id":9,"name":"proj","path":"proj","default_branch":"main","web_url":"https://gitlab.example.com/proj"}`)
			return true
		}
		return false
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	results, err := newGitlabBackend(t, srv.URL, BackendParams{}).Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 || results[0].Owner != "" {
		t.Errorf("got %+v, want one result with no owner", results)
	}
}
