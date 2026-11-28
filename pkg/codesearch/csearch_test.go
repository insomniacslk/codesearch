package codesearch

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/codesearch/index"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
)

// indexWriteMu serialises the tests that write an index. The index writer of
// github.com/google/codesearch sorts posting lists in package-level buffers,
// so two tests that run in parallel and write an index at the same time race
// on them. Reading an index has no such state.
var indexWriteMu sync.Mutex

// writeTestIndex creates the index file and lets fill add the roots and the
// files to it. Every test that writes an index must go through it, because it
// holds indexWriteMu for the whole write.
func writeTestIndex(t *testing.T, indexFile string, fill func(iw *index.IndexWriter)) {
	t.Helper()
	indexWriteMu.Lock()
	defer indexWriteMu.Unlock()
	iw := index.Create(indexFile)
	fill(iw)
	iw.Flush()
}

// TestContextLinesNeverPanics covers the regression where the after-context
// slice was taken one past the clamped upper bound, so any match near the end
// of a file with -A set panicked with "slice bounds out of range".
func TestContextLinesNeverPanics(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	writeTestIndex(t, indexFile, func(iw *index.IndexWriter) {
		iw.AddRoots([]index.Path{index.MakePath(root)})
		err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				if err := iw.AddFile(p); err != nil {
					t.Errorf("failed to index %q: %v", p, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Errorf("failed to index tree: %v", err)
		}
	})
	return indexFile, root
}

func newCsearchBackend(t *testing.T, indexFile string) *Csearch {
	t.Helper()
	b, err := NewCsearch("test", BackendParams{"index_file": indexFile})
	if err != nil {
		t.Fatalf("failed to build backend: %v", err)
	}
	return b
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
	t.Parallel()
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
			results, err := newCsearchBackend(t, indexFile).Search(context.Background(), tt.pattern, SearchOptions{SearchInFilenames: true})
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
	t.Parallel()
	indexFile, _ := newTestIndex(t, filenameTree)
	results, err := newCsearchBackend(t, indexFile).Search(context.Background(), "thing", SearchOptions{SearchInFilenames: true})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if got, want := resultPaths(results), []string{"pkg/thing/handler.go"}; !equalLines(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCsearchSearchInFilenamesUsesTheIndex: -F lists the files the index
// holds and does not look at the file system. A file that cindex never saw is
// not reported, and the search works when the tree cannot be read at all.
func TestCsearchSearchInFilenamesUsesTheIndex(t *testing.T) {
	t.Parallel()
	indexFile, root := newTestIndex(t, filenameTree)
	b := newCsearchBackend(t, indexFile)
	before, err := b.Search(context.Background(), "main.go", SearchOptions{SearchInFilenames: true})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	want := []string{"cmd/cs/main.go", "main.go"}
	if got := resultPaths(before); !equalLines(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for _, r := range before {
		if !r.IsFilename || r.RepoName != root || r.FileURL != "file://"+filepath.Join(root, r.Path) {
			t.Errorf("got %+v, want a file name result under %q", r, root)
		}
	}

	// added after the index was built
	if err := os.WriteFile(filepath.Join(root, "new_main.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	// and the whole tree gone
	if err := os.RemoveAll(filepath.Join(root, "cmd")); err != nil {
		t.Fatalf("failed to remove: %v", err)
	}
	after, err := b.Search(context.Background(), "main.go", SearchOptions{SearchInFilenames: true})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if got := resultPaths(after); !equalLines(got, want) {
		t.Errorf("got %q after the tree changed, want the indexed names %q", got, want)
	}
}

func TestRemovePathPrefix(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		s, prefix, want string
	}{
		{"/src/repo/cmd/main.go", "/src/repo", "cmd/main.go"},
		{"/src/repo/main.go", "/src/repo/", "main.go"},
		{"/src/repo", "/src/repo", ""},
		{"/other/main.go", "/src/repo", "/other/main.go"},
		{"main.go", "", "main.go"},
		{"/src/repo/main.go", "/", "src/repo/main.go"},
		{"/", "/", ""},
	} {
		if got := removePathPrefix(tt.s, tt.prefix); got != tt.want {
			t.Errorf("removePathPrefix(%q, %q) = %q, want %q", tt.s, tt.prefix, got, tt.want)
		}
	}
}

// TestHasPathPrefix covers the component-boundary comparison: a plain
// strings.HasPrefix makes "/src/repo" look like a parent of "/src/repo2".
func TestHasPathPrefix(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		s, prefix string
		want      bool
	}{
		{"/src/repo/main.go", "/src/repo", true},
		{"/src/repo", "/src/repo", true},
		{"/src/repo/main.go", "/src/repo/", true},
		{"/src/repo2/main.go", "/src/repo", false},
		{"/src/repository/main.go", "/src/repo", false},
		{"/other/main.go", "/src/repo", false},
		{"/src/repo/main.go", "", false},
		// an index rooted at / contains every absolute path
		{"/src/repo/main.go", "/", true},
		{"/", "/", true},
		{"main.go", "/", false},
	} {
		if got := hasPathPrefix(tt.s, tt.prefix); got != tt.want {
			t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", tt.s, tt.prefix, got, tt.want)
		}
	}
}

// TestRemovePathPrefixRespectsComponents is the same boundary problem as it
// shows up in the reported path: a sibling directory sharing a prefix used to
// have its name mangled instead of being left alone.
func TestRemovePathPrefixRespectsComponents(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		s, prefix, want string
	}{
		{"/src/repo2/main.go", "/src/repo", "/src/repo2/main.go"},
		{"/src/repository/a.go", "/src/repo", "/src/repository/a.go"},
		{"/src/repo/main.go", "/src/repo", "main.go"},
	} {
		if got := removePathPrefix(tt.s, tt.prefix); got != tt.want {
			t.Errorf("removePathPrefix(%q, %q) = %q, want %q", tt.s, tt.prefix, got, tt.want)
		}
	}
}

// TestCsearchHonoursCancellation checks that a local search stops when the
// caller cancels, rather than grinding through every file in the index.
func TestCsearchHonoursCancellation(t *testing.T) {
	t.Parallel()
	files := make(map[string]string, 200)
	for i := 0; i < 200; i++ {
		files[fmt.Sprintf("f%03d.go", i)] = "package main\n// needle\n"
	}
	indexFile, _ := newTestIndex(t, files)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newCsearchBackend(t, indexFile).Search(ctx, "needle", SearchOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got error %v, want context.Canceled", err)
	}
}

func TestCsearchHonoursTimeout(t *testing.T) {
	t.Parallel()
	indexFile, _ := newTestIndex(t, map[string]string{"a.go": "needle\n"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	_, err := newCsearchBackend(t, indexFile).Search(ctx, "needle", SearchOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got error %v, want context.DeadlineExceeded", err)
	}
}

func TestExpandHome(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	for _, tt := range []struct {
		name, path, want string
		wantErr          bool
	}{
		{name: "tilde alone", path: "~", want: home},
		{name: "tilde slash", path: "~/.csearchindex", want: filepath.Join(home, ".csearchindex")},
		{name: "nested", path: "~/a/b/c", want: filepath.Join(home, "a/b/c")},
		{name: "absolute is untouched", path: "/tmp/index", want: "/tmp/index"},
		{name: "relative is untouched", path: "index", want: "index"},
		{name: "empty is untouched", path: "", want: ""},
		{name: "tilde inside is untouched", path: "/tmp/~/index", want: "/tmp/~/index"},
		{name: "other user is rejected", path: "~someone/index", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandHome(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("got error %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCsearchExpandsIndexPath checks that the expansion is actually applied to
// the configured index file.
func TestCsearchExpandsIndexPath(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	b, err := NewCsearch("test", BackendParams{"index_file": "~/.csearchindex"})
	if err != nil {
		t.Fatalf("failed to build backend: %v", err)
	}
	if got, want := b.indexFile, filepath.Join(home, ".csearchindex"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCsearchMissingIndex covers index.Open calling log.Fatal internally: a
// missing or malformed index used to abort the process with "corrupt index:
// remove ..." instead of returning an error the caller could report.
func TestCsearchMissingIndex(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		setup   func(t *testing.T) string
		wantMsg string
	}{
		{
			name: "does not exist",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "nonexistent")
			},
			wantMsg: "does not exist",
		},
		{
			name: "empty file",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "empty")
				if err := os.WriteFile(p, nil, 0o644); err != nil {
					t.Fatalf("failed to write: %v", err)
				}
				return p
			},
			wantMsg: "not a codesearch index",
		},
		{
			name: "not an index",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "junk")
				if err := os.WriteFile(p, []byte("this is not an index at all"), 0o644); err != nil {
					t.Fatalf("failed to write: %v", err)
				}
				return p
			},
			wantMsg: "not a codesearch index",
		},
		{
			name:    "is a directory",
			setup:   func(t *testing.T) string { return t.TempDir() },
			wantMsg: "not a codesearch index",
		},
		// index.Open checks the size and the trailer and calls log.Fatal
		// when either is wrong, so these used to exit the test binary
		{
			name:    "header only",
			setup:   csearchWriteIndexFile(indexFormats[0].magic),
			wantMsg: "truncated",
		},
		{
			name:    "header and body without trailer",
			setup:   csearchWriteIndexFile(indexFormats[0].magic + strings.Repeat("\x00", 64)),
			wantMsg: "truncated",
		},
		{
			name:    "header and trailer only",
			setup:   csearchWriteIndexFile(indexFormats[0].magic + indexFormats[0].trailer),
			wantMsg: "truncated",
		},
		// the same for format 2, which has a different trailer
		{
			name:    "v2 header only",
			setup:   csearchWriteIndexFile(indexFormats[1].magic),
			wantMsg: "truncated",
		},
		{
			name:    "v2 header and body without trailer",
			setup:   csearchWriteIndexFile(indexFormats[1].magic + strings.Repeat("\x00", 128)),
			wantMsg: "truncated",
		},
		{
			name:    "v2 header with v1 trailer",
			setup:   csearchWriteIndexFile(indexFormats[1].magic + strings.Repeat("\x00", 128) + indexFormats[0].trailer),
			wantMsg: "truncated",
		},
		{
			name:    "format from a newer cindex",
			setup:   csearchWriteIndexFile("csearch index 9\n" + strings.Repeat("\x00", 128)),
			wantMsg: `unsupported format version "9"`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup(t)
			_, err := newCsearchBackend(t, path).Search(context.Background(), "needle", SearchOptions{})
			if err == nil {
				t.Fatal("got nil error, want a failure")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("got error %q, want it to mention %q", err, tt.wantMsg)
			}
			if !strings.Contains(err.Error(), filepath.Base(path)) {
				t.Errorf("error %q does not name the index", err)
			}
		})
	}
}

// TestCsearchValidIndexStillOpens makes sure the check does not reject a real
// index built by the index package.
func TestCsearchValidIndexStillOpens(t *testing.T) {
	t.Parallel()
	indexFile, _ := newTestIndex(t, map[string]string{"a.go": "needle here\n"})
	results, err := newCsearchBackend(t, indexFile).Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("got %d results, want 1", len(results))
	}
}

func csearchWriteIndexFile(content string) func(t *testing.T) string {
	return func(t *testing.T) string {
		p := filepath.Join(t.TempDir(), "csearchindex")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write: %v", err)
		}
		return p
	}
}

// TestCsearchSkipsStaleFiles covers a file deleted after cindex ran: the index
// is a snapshot, so this is the normal state, and it used to fail the whole
// search with "match reader failed" instead of returning the other hits.
func TestCsearchSkipsStaleFiles(t *testing.T) {
	t.Parallel()
	indexFile, root := newTestIndex(t, map[string]string{
		"a.go": "needle\n",
		"b.go": "needle\n",
		"c.go": "needle\n",
	})
	if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
		t.Fatalf("failed to remove: %v", err)
	}
	for _, opts := range []SearchOptions{{}, {LinesBefore: 1, LinesAfter: 1}} {
		results, err := newCsearchBackend(t, indexFile).Search(context.Background(), "needle", opts)
		if err != nil {
			t.Fatalf("opts %+v: search failed: %v", opts, err)
		}
		if got, want := resultPaths(results), []string{"b.go", "c.go"}; !equalLines(got, want) {
			t.Errorf("opts %+v: got %q, want %q", opts, got, want)
		}
	}
}

// TestCsearchSearchInFilenamesHonoursCancellation covers the -F walk, which
// never looked at the context, so neither --timeout nor Ctrl-C stopped it.
func TestCsearchSearchInFilenamesHonoursCancellation(t *testing.T) {
	t.Parallel()
	indexFile, _ := newTestIndex(t, filenameTree)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results, err := newCsearchBackend(t, indexFile).Search(ctx, "main.go", SearchOptions{SearchInFilenames: true})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %d results and error %v, want context.Canceled", len(results), err)
	}
}

// TestCsearchSearchInFilenamesIsOrdered covers -F results coming out of a map
// in random order, which made --limit pick a different subset on every run.
func TestCsearchSearchInFilenamesIsOrdered(t *testing.T) {
	t.Parallel()
	files := make(map[string]string, 50)
	for i := 0; i < 50; i++ {
		files[fmt.Sprintf("f%02d.go", i)] = "package main\n"
	}
	indexFile, _ := newTestIndex(t, files)

	results, err := newCsearchBackend(t, indexFile).Search(context.Background(), `\.go$`, SearchOptions{SearchInFilenames: true})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	got := make([]string, 0, len(results))
	for _, r := range results {
		got = append(got, r.Path)
	}
	if want := resultPaths(results); !equalLines(got, want) {
		t.Errorf("results are not in lexical order: %q", got)
	}
}

// TestCsearchRootIndex covers an index whose root is "/": the root used to
// match no path at all, so every search failed with "no indexed path found".
func TestCsearchRootIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("needle\n"), 0o644); err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	// index a single file under a root of "/" rather than walk the whole
	// file system
	indexFile := filepath.Join(t.TempDir(), "csearchindex")
	writeTestIndex(t, indexFile, func(iw *index.IndexWriter) {
		iw.AddRoots([]index.Path{index.MakePath("/")})
		if err := iw.AddFile(file); err != nil {
			t.Errorf("failed to index %q: %v", file, err)
		}
	})

	results, err := newCsearchBackend(t, indexFile).Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if got, want := resultPaths(results), []string{strings.TrimPrefix(file, "/")}; !equalLines(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(results) == 1 && results[0].RepoName != "/" {
		t.Errorf("got repo %q, want \"/\"", results[0].RepoName)
	}
}

// TestCsearchReadsIndexFormat2 pins the format the test indexes are written
// in: a cindex newer than v1.2.0 writes format 2, which this backend used to
// reject as "not a codesearch index".
func TestCsearchReadsIndexFormat2(t *testing.T) {
	t.Parallel()
	indexFile, _ := newTestIndex(t, map[string]string{"a.go": "needle\n"})
	data, err := os.ReadFile(indexFile)
	if err != nil {
		t.Fatalf("failed to read index: %v", err)
	}
	if !strings.HasPrefix(string(data), indexFormats[1].magic) {
		t.Fatalf("test index starts with %q, want format 2", data[:indexMagicLen])
	}
	results, err := newCsearchBackend(t, indexFile).Search(context.Background(), "needle", SearchOptions{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 || results[0].Path != "a.go" || results[0].Lineno != 1 {
		t.Errorf("got %+v, want one result for a.go line 1", results)
	}
}

// TestCsearchParallelGrepKeepsOrder: files are grepped by several workers,
// and the results must still come out in index order, with every match of a
// file together, exactly as a serial run gives them.
func TestCsearchParallelGrepKeepsOrder(t *testing.T) {
	t.Parallel()
	const fileCount = 300
	files := make(map[string]string, fileCount)
	for i := range fileCount {
		content := "needle one\nfiller\nneedle two\n"
		if i%3 == 0 {
			// the index still proposes these, as they hold every trigram
			content = "need\nedle\n"
		}
		files[fmt.Sprintf("d%02d/f%03d.go", i%7, i)] = content
	}
	indexFile, _ := newTestIndex(t, files)
	b := newCsearchBackend(t, indexFile)

	results, err := b.Search(context.Background(), "needle", SearchOptions{LinesAfter: 1})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if want := (fileCount - fileCount/3) * 2; len(results) != want {
		t.Fatalf("got %d results, want %d", len(results), want)
	}
	for i := 0; i < len(results); i += 2 {
		first, second := results[i], results[i+1]
		if first.Path != second.Path || first.Lineno != 1 || second.Lineno != 3 {
			t.Fatalf("results %d and %d are %s:%d and %s:%d, want lines 1 and 3 of one file",
				i, i+1, first.Path, first.Lineno, second.Path, second.Lineno)
		}
		if len(first.Context.After) != 1 || first.Context.After[0] != "filler" {
			t.Fatalf("result %d has context %q, want [filler]", i, first.Context.After)
		}
		if i > 0 && results[i-1].Path >= first.Path {
			t.Fatalf("result %d (%s) comes after %s, want index order", i, first.Path, results[i-1].Path)
		}
	}

	// a limit must give the leading results of the full search
	limited, err := b.Search(context.Background(), "needle", SearchOptions{Limit: 5, LinesAfter: 1})
	if err != nil {
		t.Fatalf("limited search failed: %v", err)
	}
	if len(limited) < 5 {
		t.Fatalf("got %d limited results, want at least 5", len(limited))
	}
	for i, r := range limited {
		if r.Path != results[i].Path || r.Lineno != results[i].Lineno {
			t.Errorf("limited result %d is %s:%d, want %s:%d", i, r.Path, r.Lineno, results[i].Path, results[i].Lineno)
		}
	}
}

// TestCsearchCorruptSectionOffsets: an index whose section table points
// outside of the file, or backwards, used to reach log.Fatal in the library
// and exit the process. It is now an error. If the check is removed, this
// test does not fail, it kills the test binary.
func TestCsearchCorruptSectionOffsets(t *testing.T) {
	t.Parallel()
	indexFile, _ := newTestIndex(t, map[string]string{"a.go": "needle\n"})
	good, err := os.ReadFile(indexFile)
	if err != nil {
		t.Fatalf("failed to read index: %v", err)
	}
	v2 := indexFormats[1]
	tableStart := len(good) - len(v2.trailer) - v2.fields*v2.fieldSize
	// field is the position of an 8-byte entry in the section table
	corrupt := func(field int, value uint64) string {
		data := append([]byte(nil), good...)
		binary.BigEndian.PutUint64(data[tableStart+field*8:], value)
		return string(data)
	}
	v1 := indexFormats[0]
	v1Table := make([]byte, v1.fields*v1.fieldSize)
	for i := range v1.fields {
		// every section "starts" far beyond the end of this small file
		binary.BigEndian.PutUint32(v1Table[i*4:], 1<<30)
	}

	for name, content := range map[string]string{
		"path list beyond the end":      corrupt(0, uint64(len(good))+1000),
		"posting index inside trailer":  corrupt(7, uint64(tableStart)+1),
		"name list before the header":   corrupt(2, 3),
		"sections out of order":         corrupt(4, 16),
		"offset with the top bit set":   corrupt(6, 1<<63),
		"format 1 with wild offsets":    v1.magic + strings.Repeat("\x00", 64) + string(v1Table) + v1.trailer,
		"format 1 with a table of zero": v1.magic + strings.Repeat("\x00", 64) + strings.Repeat("\x00", 20) + v1.trailer,
	} {
		t.Run(name, func(t *testing.T) {
			path := csearchWriteIndexFile(content)(t)
			_, err := newCsearchBackend(t, path).Search(context.Background(), "needle", SearchOptions{})
			if err == nil {
				t.Fatal("got nil error, want a failure")
			}
			if !strings.Contains(err.Error(), "corrupt") || !strings.Contains(err.Error(), "cindex") {
				t.Errorf("got error %q, want it to say the index is corrupt and name cindex", err)
			}
		})
	}

	// the untouched index still opens
	if _, err := newCsearchBackend(t, csearchWriteIndexFile(string(good))(t)).Search(context.Background(), "needle", SearchOptions{}); err != nil {
		t.Errorf("the valid index failed: %v", err)
	}
}

// format1Index is a committed index in format 1, see testdata/README.md.
const format1Index = "testdata/index-format-1"

// TestCsearchFormat1Fixture guards the fixture itself.
func TestCsearchFormat1Fixture(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(format1Index)
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}
	if !strings.HasPrefix(string(data), indexFormats[0].magic) || !strings.HasSuffix(string(data), indexFormats[0].trailer) {
		t.Fatal("the fixture is not a format 1 index")
	}
}

// TestCsearchFormat1Filenames: the roots and the names of a format 1 index
// are read correctly.
func TestCsearchFormat1Filenames(t *testing.T) {
	t.Parallel()
	b := newCsearchBackend(t, format1Index)
	for _, tt := range []struct {
		pattern string
		want    []string
	}{
		{`.`, []string{"cmd/main.go", "lib/haystack.go", "lib/needle.go"}},
		{`^lib/`, []string{"lib/haystack.go", "lib/needle.go"}},
		{`needle`, []string{"lib/needle.go"}},
		{`\.md$`, nil},
	} {
		results, err := b.Search(context.Background(), tt.pattern, SearchOptions{SearchInFilenames: true})
		if err != nil {
			t.Fatalf("%q: search failed: %v", tt.pattern, err)
		}
		if got := resultPaths(results); !equalLines(got, tt.want) {
			t.Errorf("%q: got %q, want %q", tt.pattern, got, tt.want)
		}
		for _, r := range results {
			if r.RepoName != "/fixture" || r.FileURL != "file:///fixture/"+r.Path {
				t.Errorf("%q: got %+v, want a file under /fixture", tt.pattern, r)
			}
		}
	}
}

// TestCsearchFormat1PostingLists: the posting lists of a format 1 index pick
// the right candidate files. The files of the fixture do not exist, so every
// candidate ends as a "skipping" warning, and the warnings say which files
// the index proposed.
func TestCsearchFormat1PostingLists(t *testing.T) {
	t.Parallel()
	b := newCsearchBackend(t, format1Index)
	for _, tt := range []struct {
		pattern string
		want    []string
	}{
		{"needle", []string{"/fixture/cmd/main.go", "/fixture/lib/needle.go"}},
		{"nothing to find", []string{"/fixture/lib/haystack.go"}},
		{"package", []string{"/fixture/cmd/main.go", "/fixture/lib/haystack.go", "/fixture/lib/needle.go"}},
		{"absent from every file", nil},
	} {
		log, hook := logrustest.NewNullLogger()
		results, err := b.Search(context.Background(), tt.pattern, SearchOptions{Log: log})
		if err != nil {
			t.Fatalf("%q: search failed: %v", tt.pattern, err)
		}
		if len(results) != 0 {
			t.Errorf("%q: got %d results for files that do not exist", tt.pattern, len(results))
		}
		var candidates []string
		for _, e := range hook.AllEntries() {
			if name, ok := strings.CutPrefix(e.Message, `Skipping "`); ok && e.Level == logrus.WarnLevel {
				candidates = append(candidates, name[:strings.Index(name, `"`)])
			}
		}
		if !equalLines(candidates, tt.want) {
			t.Errorf("%q: index proposed %q, want %q", tt.pattern, candidates, tt.want)
		}
	}
}

// TestCsearchFormat1CorruptOffsets: the section table check on a real format
// 1 index, where the offsets are 4 bytes each.
func TestCsearchFormat1CorruptOffsets(t *testing.T) {
	t.Parallel()
	good, err := os.ReadFile(format1Index)
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}
	v1 := indexFormats[0]
	tableStart := len(good) - len(v1.trailer) - v1.fields*v1.fieldSize
	for field := range v1.fields {
		data := append([]byte(nil), good...)
		binary.BigEndian.PutUint32(data[tableStart+field*4:], uint32(len(good))+7)
		path := csearchWriteIndexFile(string(data))(t)
		_, err := newCsearchBackend(t, path).Search(context.Background(), "needle", SearchOptions{})
		if err == nil || !strings.Contains(err.Error(), "corrupt") {
			t.Errorf("field %d: got error %v, want the index reported as corrupt", field, err)
		}
	}
}

func TestRootOf(t *testing.T) {
	t.Parallel()
	roots := []string{"/src/repo", "/src/repo/vendor/lib", "/other/"}
	for _, tc := range []struct{ name, want string }{
		{"/src/repo/a.go", "/src/repo"},
		{"/src/repo/vendor/lib/b.go", "/src/repo/vendor/lib"},
		{"/other/c.go", "/other/"},
		// the root that cindex dropped behind "/src/repo"; a deeper dropped
		// root comes back as its first component
		{"/src/repo-mail/sub/g.go", "/src/repo-mail"},
		{"/src/repo-mail/docs/d.md", "/src/repo-mail"},
		{"/src/repo2/e.go", "/src/repo2"},
		// a file directly next to a root has no component to extend to
		{"/src/repo.txt", "/src"},
		{"/elsewhere/f.go", "/elsewhere"},
	} {
		got, held := rootOf(roots, tc.name)
		if got != tc.want {
			t.Errorf("rootOf(%q) = %q, want %q", tc.name, got, tc.want)
		}
		if wantHeld := longestRoot(roots, tc.name) != ""; held != wantHeld {
			t.Errorf("rootOf(%q) reports held=%v, want %v", tc.name, held, wantHeld)
		}
	}
}

// TestCsearchFileOutsideRoots covers an index as cindex leaves it after a
// merge: the files of "repo-mail" are there, but its root is not, because the
// root "repo" before it is a string prefix. Such a file failed the search.
func TestCsearchFileOutsideRoots(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	kept := filepath.Join(dir, "repo", "a.go")
	dropped := filepath.Join(dir, "repo-mail", "docs", "b.go")
	for _, file := range []string{kept, dropped} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		if err := os.WriteFile(file, []byte("needle\n"), 0o644); err != nil {
			t.Fatalf("failed to write: %v", err)
		}
	}
	indexFile := filepath.Join(t.TempDir(), "csearchindex")
	writeTestIndex(t, indexFile, func(iw *index.IndexWriter) {
		iw.AddRoots([]index.Path{index.MakePath(filepath.Join(dir, "repo"))})
		for _, file := range []string{kept, dropped} {
			if err := iw.AddFile(file); err != nil {
				t.Errorf("failed to index %q: %v", file, err)
			}
		}
	})

	want := map[string]string{
		"a.go":      "file://" + filepath.Join(dir, "repo"),
		"docs/b.go": "file://" + filepath.Join(dir, "repo-mail"),
	}
	for _, opts := range []SearchOptions{{}, {SearchInFilenames: true}} {
		pattern := "needle"
		if opts.SearchInFilenames {
			pattern = `\.go$`
		}
		results, err := newCsearchBackend(t, indexFile).Search(context.Background(), pattern, opts)
		if err != nil {
			t.Fatalf("search (filenames=%v) failed: %v", opts.SearchInFilenames, err)
		}
		got := make(map[string]string)
		for _, r := range results {
			got[r.Path] = r.RepoURL
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("filenames=%v: got %v, want %v", opts.SearchInFilenames, got, want)
		}
	}
}
