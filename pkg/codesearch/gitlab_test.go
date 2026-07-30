package codesearch

import (
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
	g.paths = append(g.paths, r.URL.Path)
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if g.handler != nil && g.handler(w, r) {
		return
	}
	fmt.Fprint(w, `[]`)
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

// TestGitlabUnknownProject covers the missing not-found check: projectID
// started at 0 and was never validated, so a project that could not be
// resolved silently issued a blob search against project 0.
func TestGitlabUnknownProject(t *testing.T) {
	rec := &gitlabRecorder{handler: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v4/projects" {
			fmt.Fprint(w, `[{"id":42,"name":"other","path":"other"}]`)
			return true
		}
		return false
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	_, err := newGitlabBackend(t, srv.URL, BackendParams{"project": "missing"}).Search("needle")
	if err == nil {
		t.Fatal("got nil error, want a not-found failure")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("got error %q, want it to name the project", err)
	}
	if rec.requested("/projects/0/") {
		t.Error("searched project 0 instead of failing")
	}
}

func TestGitlabUnknownGroup(t *testing.T) {
	rec := &gitlabRecorder{handler: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v4/groups" {
			fmt.Fprint(w, `[{"id":7,"name":"other","full_path":"other"}]`)
			return true
		}
		return false
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	_, err := newGitlabBackend(t, srv.URL, BackendParams{"group": "missing"}).Search("needle")
	if err == nil {
		t.Fatal("got nil error, want a not-found failure")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("got error %q, want it to name the group", err)
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
