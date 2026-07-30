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

	sensitive, err := newCsearchBackend(t, indexFile).Search(context.Background(), "needle")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(sensitive) != 0 {
		t.Errorf("got %d results for a case-sensitive search, want 0", len(sensitive))
	}

	insensitive, err := newCsearchBackend(t, indexFile).Search(context.Background(), "needle", WithCaseInsensitive(true))
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(insensitive) != 1 {
		t.Errorf("got %d results for a case-insensitive search, want 1", len(insensitive))
	}
}
