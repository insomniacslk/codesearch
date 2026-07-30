package codesearch

import (
	"fmt"
	"strings"
	"testing"
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
