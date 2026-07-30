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
