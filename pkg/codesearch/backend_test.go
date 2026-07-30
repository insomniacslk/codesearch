package codesearch

import (
	"context"
	"testing"
)

// TestBackendCapabilities pins which options each backend actually implements.
// Both GitHub and GitLab used to accept -i and -F, store them in a field and
// never read it, so the search silently answered a different question.
func TestBackendCapabilities(t *testing.T) {
	for _, tt := range []struct {
		backendType       BackendType
		caseInsensitive   bool
		searchInFilenames bool
	}{
		{BackendTypeGithub, false, false},
		{BackendTypeGitlab, false, false},
		{BackendTypeCsearch, true, true},
	} {
		t.Run(string(tt.backendType), func(t *testing.T) {
			b := BackendByType(tt.backendType)
			if b == nil {
				t.Fatalf("no backend for type %q", tt.backendType)
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
