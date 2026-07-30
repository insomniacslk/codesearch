package codesearch

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/codesearch/index"
)

// TestContextLinesNeverPanics covers the regression where the after-context
// slice was taken one past the clamped upper bound, so any match near the end
// of a file with -A set panicked with "slice bounds out of range".
func TestContextLinesNeverPanics(t *testing.T) {
	files := [][]string{
		{},
		{""},
		{"one"},
		{"one", "two"},
		{"one", "two", "three"},
		{"one", "two", "three", "four", "five"},
	}
	for _, lines := range files {
		// deliberately includes line numbers past the end of the file
		for lineno := -2; lineno <= len(lines)+2; lineno++ {
			for before := 0; before <= 4; before++ {
				for after := 0; after <= 4; after++ {
					name := fmt.Sprintf("len=%d/lineno=%d/B=%d/A=%d",
						len(lines), lineno, before, after)
					t.Run(name, func(t *testing.T) {
						defer func() {
							if r := recover(); r != nil {
								t.Fatalf("panicked: %v", r)
							}
						}()
						contextLines(lines, lineno, before, after)
					})
				}
			}
		}
	}
}

// TestContextLinesLastLineWithAfter is the exact case reported: a match on the
// final line of the file with one line of trailing context requested.
func TestContextLinesLastLineWithAfter(t *testing.T) {
	lines := strings.Split("aaa\nbbb\nccc", "\n")
	before, after := contextLines(lines, 3, 0, 1)
	if len(before) != 0 {
		t.Errorf("before: got %q, want empty", before)
	}
	if len(after) != 0 {
		t.Errorf("after: got %q, want empty", after)
	}
}

// TestContextLinesSelectsTheRightLines covers the off-by-one where the
// before-context ended at the match line itself (including it, and dropping
// the earliest requested line) and the after-context started one line too
// late, skipping the line immediately following the match.
func TestContextLinesSelectsTheRightLines(t *testing.T) {
	lines := []string{"one", "two", "three", "four", "five"}
	for _, tt := range []struct {
		name                  string
		lineno, before, after int
		wantBefore, wantAfter []string
	}{
		{
			name:   "middle of file",
			lineno: 3, before: 1, after: 1,
			wantBefore: []string{"two"}, wantAfter: []string{"four"},
		},
		{
			name:   "two lines each side",
			lineno: 3, before: 2, after: 2,
			wantBefore: []string{"one", "two"}, wantAfter: []string{"four", "five"},
		},
		{
			name:   "no context requested",
			lineno: 3, before: 0, after: 0,
			wantBefore: nil, wantAfter: nil,
		},
		{
			name:   "first line clamps before",
			lineno: 1, before: 2, after: 2,
			wantBefore: nil, wantAfter: []string{"two", "three"},
		},
		{
			name:   "last line clamps after",
			lineno: 5, before: 2, after: 2,
			wantBefore: []string{"three", "four"}, wantAfter: nil,
		},
		{
			name:   "context larger than file",
			lineno: 3, before: 99, after: 99,
			wantBefore: []string{"one", "two"}, wantAfter: []string{"four", "five"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before, after := contextLines(lines, tt.lineno, tt.before, tt.after)
			if !equalLines(before, tt.wantBefore) {
				t.Errorf("before: got %q, want %q", before, tt.wantBefore)
			}
			if !equalLines(after, tt.wantAfter) {
				t.Errorf("after: got %q, want %q", after, tt.wantAfter)
			}
		})
	}
}

// TestContextLinesExcludesTheMatchLine is the core invariant: the matching
// line is printed separately, so it must never appear in either context.
func TestContextLinesExcludesTheMatchLine(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}
	for lineno := 1; lineno <= len(lines); lineno++ {
		before, after := contextLines(lines, lineno, 10, 10)
		match := lines[lineno-1]
		for _, l := range append(append([]string{}, before...), after...) {
			if l == match {
				t.Errorf("lineno %d: match line %q appears in its own context", lineno, match)
			}
		}
		if got, want := len(before)+len(after), len(lines)-1; got != want {
			t.Errorf("lineno %d: got %d context lines, want %d", lineno, got, want)
		}
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestClamp(t *testing.T) {
	for _, tt := range []struct {
		v, lo, hi, want int
	}{
		{5, 0, 10, 5},
		{-1, 0, 10, 0},
		{11, 0, 10, 10},
		{0, 0, 0, 0},
		{5, 3, 3, 3},
	} {
		if got := clamp(tt.v, tt.lo, tt.hi); got != tt.want {
			t.Errorf("clamp(%d, %d, %d) = %d, want %d",
				tt.v, tt.lo, tt.hi, got, tt.want)
		}
	}
}

// newTestIndex writes files into a temporary tree and builds a real cindex
// over it, so the csearch backend can be exercised end to end.
func newTestIndex(t *testing.T, files map[string]string) (indexFile, root string) {
	t.Helper()
	root = t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("failed to create dir for %q: %v", name, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write %q: %v", name, err)
		}
	}
	indexFile = filepath.Join(t.TempDir(), "csearchindex")
	iw := index.Create(indexFile)
	iw.AddPaths([]string{root})
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			iw.AddFile(p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to index tree: %v", err)
	}
	iw.Flush()
	return indexFile, root
}

func newCsearchBackend(t *testing.T, indexFile string) *Csearch {
	t.Helper()
	b, err := (&Csearch{}).New("test", BackendParams{"index_file": indexFile})
	if err != nil {
		t.Fatalf("failed to build backend: %v", err)
	}
	return b.(*Csearch)
}

func resultPaths(results Results) []string {
	paths := make([]string, 0, len(results))
	for _, r := range results {
		paths = append(paths, r.Path)
	}
	sort.Strings(paths)
	return paths
}

var filenameTree = map[string]string{
	"main.go":              "package main\n",
	"cmd/cs/main.go":       "package main\n",
	"pkg/thing/handler.go": "package thing\n",
	"README.md":            "docs\n",
}

func TestCsearchSearchInFilenames(t *testing.T) {
	indexFile, _ := newTestIndex(t, filenameTree)
	for _, tt := range []struct {
		name, pattern string
		want          []string
	}{
		{"base name", "handler.go", []string{"pkg/thing/handler.go"}},
		{"matches every path component", "main.go", []string{"cmd/cs/main.go", "main.go"}},
		{"directory component", "cmd/cs", []string{"cmd/cs/main.go"}},
		{"extension", `\.md$`, []string{"README.md"}},
		{"no match", "nosuchfile", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			results, err := newCsearchBackend(t, indexFile).Search(tt.pattern, WithSearchInFilenames(true))
			if err != nil {
				t.Fatalf("search failed: %v", err)
			}
			if got := resultPaths(results); !equalLines(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCsearchSearchInFilenamesSkipsDirectories checks that a pattern matching
// a directory name does not report the directory itself as a hit.
func TestCsearchSearchInFilenamesSkipsDirectories(t *testing.T) {
	indexFile, _ := newTestIndex(t, filenameTree)
	results, err := newCsearchBackend(t, indexFile).Search("thing", WithSearchInFilenames(true))
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if got, want := resultPaths(results), []string{"pkg/thing/handler.go"}; !equalLines(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCsearchSearchInFilenamesUnreadableDir is the regression test for the
// walk callback dereferencing info before checking err. filepath.Walk passes a
// nil info alongside the error for a directory it cannot read.
func TestCsearchSearchInFilenamesUnreadableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, permissions are not enforced")
	}
	indexFile, root := newTestIndex(t, filenameTree)

	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatalf("failed to create locked dir: %v", err)
	}
	// readable but not traversable: Walk can list the entries but cannot
	// lstat them, which is the case where it passes a nil info alongside err
	if err := os.Chmod(locked, 0o400); err != nil {
		t.Fatalf("failed to chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	results, err := newCsearchBackend(t, indexFile).Search("main.go", WithSearchInFilenames(true))
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	// the readable part of the tree is still reported
	if got, want := resultPaths(results), []string{"cmd/cs/main.go", "main.go"}; !equalLines(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRemovePathPrefix(t *testing.T) {
	for _, tt := range []struct {
		s, prefix, want string
	}{
		{"/src/repo/cmd/main.go", "/src/repo", "cmd/main.go"},
		{"/src/repo/main.go", "/src/repo/", "main.go"},
		{"/src/repo", "/src/repo", ""},
		{"/other/main.go", "/src/repo", "/other/main.go"},
		{"main.go", "", "main.go"},
	} {
		if got := removePathPrefix(tt.s, tt.prefix); got != tt.want {
			t.Errorf("removePathPrefix(%q, %q) = %q, want %q", tt.s, tt.prefix, got, tt.want)
		}
	}
}
