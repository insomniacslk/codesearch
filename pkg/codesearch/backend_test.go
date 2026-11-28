package codesearch

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestBackendCapabilities pins which options each backend actually implements.
// Both GitHub and GitLab used to accept -i and -F, store them in a field and
// never read it, so the search silently answered a different question.
func TestBackendCapabilities(t *testing.T) {
	for _, tt := range []struct {
		backendType       BackendType
		params            BackendParams
		caseInsensitive   bool
		searchInFilenames bool
	}{
		{BackendTypeGithub, BackendParams{"org": "o", "token": "t", "api_endpoint": "https://api.github.com"}, false, false},
		{BackendTypeGitlab, BackendParams{"token": "t", "api_endpoint": "https://gitlab.com/api/v4"}, false, false},
		{BackendTypeCsearch, BackendParams{"index_file": "/nonexistent"}, true, true},
	} {
		t.Run(string(tt.backendType), func(t *testing.T) {
			b, err := NewBackend("test", BackendConfig{Type: tt.backendType, Params: tt.params})
			if err != nil {
				t.Fatalf("failed to build backend: %v", err)
			}
			if got := b.Type(); got != tt.backendType {
				t.Errorf("Type: got %q, want %q", got, tt.backendType)
			}
			if got := b.Supports(CapCaseInsensitive); got != tt.caseInsensitive {
				t.Errorf("CapCaseInsensitive: got %v, want %v", got, tt.caseInsensitive)
			}
			if got := b.Supports(CapSearchInFilenames); got != tt.searchInFilenames {
				t.Errorf("CapSearchInFilenames: got %v, want %v", got, tt.searchInFilenames)
			}
		})
	}
}

// TestNewBackendUnimplementedType checks that the error names both the backend
// and the type, for a type the config knows and for one it does not.
func TestNewBackendUnimplementedType(t *testing.T) {
	for _, typ := range []BackendType{BackendTypeBitbucket, "svn", ""} {
		b, err := NewBackend("mine", BackendConfig{Type: typ})
		if err == nil {
			t.Fatalf("type %q: got backend %v, want an error", typ, b)
		}
		for _, want := range []string{`"mine"`, fmt.Sprintf("%q", typ)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("type %q: error %q does not contain %s", typ, err, want)
			}
		}
	}
}

// TestNewBackendFailureIsNil checks that a constructor failure comes back as a
// nil interface, not as a Backend wrapping a nil pointer.
func TestNewBackendFailureIsNil(t *testing.T) {
	b, err := NewBackend("mine", BackendConfig{Type: BackendTypeGithub, Params: BackendParams{}})
	if err == nil {
		t.Fatal("got nil error, want a failure for missing parameters")
	}
	if b != nil {
		t.Errorf("got %#v, want a nil Backend", b)
	}
	if !strings.Contains(err.Error(), `"mine"`) {
		t.Errorf("error %q does not name the backend", err)
	}
}

// TestBackendConstructorsAreKnownTypes keeps the registry and the list of
// recognised types consistent: an implemented type the config does not
// recognise would be reported as unknown by Validate.
func TestBackendConstructorsAreKnownTypes(t *testing.T) {
	for typ := range backendConstructors {
		if !slices.Contains(knownBackendTypes, typ) {
			t.Errorf("type %q has a constructor but is not in knownBackendTypes", typ)
		}
	}
}

// TestCsearchConsumesSupportedCapabilities checks that csearch, which claims
// both capabilities, really does act on them.
func TestCsearchConsumesSupportedCapabilities(t *testing.T) {
	indexFile, _ := newTestIndex(t, map[string]string{"a.go": "NEEDLE in the haystack\n"})

	sensitive, err := newCsearchBackend(t, indexFile).Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(sensitive) != 0 {
		t.Errorf("got %d results for a case-sensitive search, want 0", len(sensitive))
	}

	insensitive, err := newCsearchBackend(t, indexFile).Search(context.Background(), "needle", SearchOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(insensitive) != 1 {
		t.Errorf("got %d results for a case-insensitive search, want 1", len(insensitive))
	}
}

// TestSearchOptionsDoNotLeakBetweenCalls checks that options are scoped to the
// call. They used to be applied by mutating the backend, so one search left
// its settings behind for the next one.
func TestSearchOptionsDoNotLeakBetweenCalls(t *testing.T) {
	indexFile, _ := newTestIndex(t, map[string]string{"a.go": "NEEDLE here\n"})
	b := newCsearchBackend(t, indexFile)

	insensitive, err := b.Search(context.Background(), "needle", SearchOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(insensitive) != 1 {
		t.Fatalf("got %d results case-insensitively, want 1", len(insensitive))
	}

	// the same instance, without the option: it must not still be in effect
	sensitive, err := b.Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(sensitive) != 0 {
		t.Errorf("got %d results case-sensitively, want 0: options leaked from the previous call", len(sensitive))
	}
}

// TestConcurrentSearchesOnOneBackend checks that a single backend instance can
// serve overlapping searches with different options, which is what makes the
// concurrent fan-out safe.
func TestConcurrentSearchesOnOneBackend(t *testing.T) {
	indexFile, _ := newTestIndex(t, map[string]string{
		"a.go": "NEEDLE upper\n",
		"b.go": "needle lower\n",
	})
	b := newCsearchBackend(t, indexFile)

	type result struct {
		insensitive bool
		count       int
		err         error
	}
	const runs = 16
	ch := make(chan result, runs)
	for i := range runs {
		go func(insensitive bool) {
			res, err := b.Search(context.Background(), "needle",
				SearchOptions{CaseInsensitive: insensitive})
			ch <- result{insensitive: insensitive, count: len(res), err: err}
		}(i%2 == 0)
	}
	for range runs {
		r := <-ch
		if r.err != nil {
			t.Fatalf("search failed: %v", r.err)
		}
		want := 1
		if r.insensitive {
			want = 2
		}
		if r.count != want {
			t.Errorf("case-insensitive=%v: got %d results, want %d", r.insensitive, r.count, want)
		}
	}
}

// TestBackendsPassOnResultsInOrder covers the OnResult contract for the
// backends that call it: what they pass on while they search is exactly what
// they return, in the same order.
func TestBackendsPassOnResultsInOrder(t *testing.T) {
	var searches, contents int32
	srv := pagedGithubServer(t, 3, 10, &searches, &contents)
	defer srv.Close()
	files := map[string]string{}
	for i := range 40 {
		files[fmt.Sprintf("d%d/f%02d.go", i%3, i)] = "needle\nother\nneedle again\n"
	}
	indexFile, _ := newTestIndex(t, files)

	for name, b := range map[string]Backend{
		"github":  newGithubBackend(t, srv.URL),
		"csearch": newCsearchBackend(t, indexFile),
	} {
		for _, limit := range []uint{0, 7} {
			t.Run(fmt.Sprintf("%s/limit=%d", name, limit), func(t *testing.T) {
				var passedOn Results
				results, err := b.Search(context.Background(), "needle", SearchOptions{
					Limit:    limit,
					OnResult: func(r Result) { passedOn = append(passedOn, r) },
				})
				if err != nil {
					t.Fatalf("search failed: %v", err)
				}
				if len(results) == 0 {
					t.Fatal("got no results")
				}
				if len(passedOn) != len(results) {
					t.Fatalf("got %d results passed on, want the %d that were returned", len(passedOn), len(results))
				}
				for i := range results {
					if passedOn[i].Path != results[i].Path || passedOn[i].Lineno != results[i].Lineno {
						t.Fatalf("result %d passed on is %s:%d, returned is %s:%d",
							i, passedOn[i].Path, passedOn[i].Lineno, results[i].Path, results[i].Lineno)
					}
				}
			})
		}
	}
}
