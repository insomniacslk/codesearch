package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatih/color"
	"github.com/insomniacslk/codesearch/pkg/codesearch"
)

// disableColor makes the printed output plain for the length of a test.
func disableColor(t *testing.T) {
	t.Helper()
	old := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = old })
}

// lockedBuffer is a bytes.Buffer that a test can read while the code under
// test writes to it from other goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls until the buffer holds want.
func (b *lockedBuffer) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(b.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("output never held %q, got:\n%s", want, b.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// streamingBackend passes the first `early` of its results to OnResult, then
// waits to be released, then returns all of its results, or err.
type streamingBackend struct {
	name    string
	results codesearch.Results
	early   int
	release chan struct{}
	err     error
}

func (s *streamingBackend) Name() string                        { return s.name }
func (s *streamingBackend) Type() codesearch.BackendType        { return "fake" }
func (s *streamingBackend) Supports(codesearch.Capability) bool { return true }
func (s *streamingBackend) Search(_ context.Context, _ string, opts codesearch.SearchOptions) (codesearch.Results, error) {
	for _, r := range s.results[:s.early] {
		if opts.OnResult != nil {
			opts.OnResult(r)
		}
	}
	if s.release != nil {
		<-s.release
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.results, nil
}

func namedResults(backend string, paths ...string) codesearch.Results {
	var out codesearch.Results
	for i, p := range paths {
		out = append(out, codesearch.Result{Backend: backend, Path: p, Line: "needle " + p, Lineno: i + 1, Highlight: [2]int{0, 6}})
	}
	return out
}

// printedAtTheEnd is the output of the path that does not print live.
func printedAtTheEnd(backends []*streamingBackend, limit uint, match *regexp.Regexp) string {
	var buf bytes.Buffer
	for _, b := range backends {
		for _, res := range selectResults(b.results, limit, match) {
			printResult(&buf, res, false)
		}
	}
	return buf.String()
}

// TestLivePrinterShowsResultsDuringTheSearch: results a backend passes on are
// on the output before its search returns.
func TestLivePrinterShowsResultsDuringTheSearch(t *testing.T) {
	disableColor(t)
	b := &streamingBackend{name: "slow", results: namedResults("slow", "a.go", "b.go", "c.go"), early: 2, release: make(chan struct{})}
	var out lockedBuffer
	live := newLivePrinter(&out, 1, 0, nil)
	done := make(chan []searchOutcome, 1)
	go func() {
		done <- runSearchesLive(context.Background(), []codesearch.Backend{b}, "needle", codesearch.SearchOptions{}, live)
	}()

	out.waitFor(t, "needle b.go")
	if strings.Contains(out.String(), "c.go") {
		t.Errorf("c.go was printed before the backend returned it:\n%s", out.String())
	}
	close(b.release)
	<-done
	if got, want := out.String(), printedAtTheEnd([]*streamingBackend{b}, 0, nil); got != want {
		t.Errorf("got output:\n%s\nwant:\n%s", got, want)
	}
	if got := live.shown(0); got != 3 {
		t.Errorf("got %d results shown, want 3", got)
	}
}

// TestLivePrinterKeepsBackendOrder: a backend that finishes early is held
// back until the backends before it are done, so the output is the same as
// when everything is printed at the end.
func TestLivePrinterKeepsBackendOrder(t *testing.T) {
	disableColor(t)
	first := &streamingBackend{name: "first", results: namedResults("first", "a.go", "b.go"), early: 1, release: make(chan struct{})}
	second := &streamingBackend{name: "second", results: namedResults("second", "c.go", "d.go"), early: 2}
	third := &streamingBackend{name: "third", results: namedResults("third", "e.go"), early: 0}
	all := []*streamingBackend{first, second, third}
	var out lockedBuffer
	live := newLivePrinter(&out, 3, 0, nil)
	done := make(chan []searchOutcome, 1)
	go func() {
		done <- runSearchesLive(context.Background(), []codesearch.Backend{first, second, third}, "needle", codesearch.SearchOptions{}, live)
	}()

	out.waitFor(t, "needle a.go")
	// give the other two every chance to print out of turn
	time.Sleep(50 * time.Millisecond)
	if got := out.String(); strings.Contains(got, "second") || strings.Contains(got, "third") {
		t.Errorf("a later backend was printed while the first one was still searching:\n%s", got)
	}
	close(first.release)
	<-done
	if got, want := out.String(), printedAtTheEnd(all, 0, nil); got != want {
		t.Errorf("got output:\n%s\nwant:\n%s", got, want)
	}
}

// TestLivePrinterSelectsLikeSelectResults: the file name filter, the limit
// and the exclusion of file name results give the same output whether a
// backend passes on none, some or all of its results early.
func TestLivePrinterSelectsLikeSelectResults(t *testing.T) {
	disableColor(t)
	results := namedResults("b", "a.go", "b.md", "c.go", "d.go", "e.txt", "f.go", "g.go")
	results[2].IsFilename = true
	for _, tt := range []struct {
		name  string
		limit uint
		match *regexp.Regexp
	}{
		{"no selection", 0, nil},
		{"limit", 2, nil},
		{"filter", 0, regexp.MustCompile(`\.go$`)},
		{"filter and limit", 3, regexp.MustCompile(`\.go$`)},
		{"limit above the count", 50, nil},
	} {
		for early := 0; early <= len(results); early++ {
			t.Run(fmt.Sprintf("%s/early=%d", tt.name, early), func(t *testing.T) {
				b := &streamingBackend{name: "b", results: results, early: early}
				var out lockedBuffer
				live := newLivePrinter(&out, 1, tt.limit, tt.match)
				runSearchesLive(context.Background(), []codesearch.Backend{b}, "needle", codesearch.SearchOptions{}, live)
				want := printedAtTheEnd([]*streamingBackend{b}, tt.limit, tt.match)
				if got := out.String(); got != want {
					t.Errorf("got output:\n%s\nwant:\n%s", got, want)
				}
				if got, want := live.shown(0), len(selectResults(results, tt.limit, tt.match)); got != want {
					t.Errorf("got %d results shown, want %d", got, want)
				}
			})
		}
	}
}

// TestLivePrinterBackendFailsAfterResults: what a backend found before it
// failed stays on the output and is counted, the failure is still reported,
// and the next backend is printed after it.
func TestLivePrinterBackendFailsAfterResults(t *testing.T) {
	disableColor(t)
	failing := &streamingBackend{name: "failing", results: namedResults("failing", "a.go", "b.go"), early: 1, err: errors.New("boom")}
	working := &streamingBackend{name: "working", results: namedResults("working", "c.go")}
	var out lockedBuffer
	live := newLivePrinter(&out, 2, 0, nil)
	outcomes := runSearchesLive(context.Background(), []codesearch.Backend{failing, working}, "needle", codesearch.SearchOptions{}, live)

	if outcomes[0].err == nil || outcomes[1].err != nil {
		t.Fatalf("got errors %v and %v, want only the first backend to fail", outcomes[0].err, outcomes[1].err)
	}
	got := out.String()
	if !strings.Contains(got, "needle a.go") || strings.Contains(got, "b.go") {
		t.Errorf("want a.go and not b.go from the failed backend, got:\n%s", got)
	}
	if strings.Index(got, "needle c.go") < strings.Index(got, "needle a.go") {
		t.Errorf("want the working backend after the failed one, got:\n%s", got)
	}
	if live.shown(0) != 1 || live.shown(1) != 1 {
		t.Errorf("got %d and %d results shown, want 1 and 1", live.shown(0), live.shown(1))
	}
}

// TestCanPrintLive: the modes that need every result first do not print live.
func TestCanPrintLive(t *testing.T) {
	if !canPrintLive(settings{}) {
		t.Error("got false for a plain search, want true")
	}
	for name, s := range map[string]settings{
		"--json": {json: true},
		"--sort": {sort: "a-z"},
		"-F":     {searchInFilenames: true},
	} {
		if canPrintLive(s) {
			t.Errorf("got true with %s, want false", name)
		}
	}
}

// blockedWriter accepts nothing until it is released, like the stdout of a
// cs whose pager is not being scrolled.
type blockedWriter struct {
	release chan struct{}
	lockedBuffer
}

func (b *blockedWriter) Write(p []byte) (int, error) {
	<-b.release
	return b.lockedBuffer.Write(p)
}

// returningBackend passes all of its results on and says when Search is over.
type returningBackend struct {
	streamingBackend
	returned chan struct{}
}

func (r *returningBackend) Search(ctx context.Context, q string, opts codesearch.SearchOptions) (codesearch.Results, error) {
	defer close(r.returned)
	return r.streamingBackend.Search(ctx, q, opts)
}

// TestLivePrinterDoesNotHoldUpTheSearch: a backend hands its results over
// without waiting for the output. An output that stands still must not keep
// the search running, where it would count against --timeout.
func TestLivePrinterDoesNotHoldUpTheSearch(t *testing.T) {
	disableColor(t)
	b := &returningBackend{
		streamingBackend: streamingBackend{name: "b", results: namedResults("b", "a.go", "b.go", "c.go"), early: 3},
		returned:         make(chan struct{}),
	}
	out := &blockedWriter{release: make(chan struct{})}
	live := newLivePrinter(out, 1, 0, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSearchesLive(context.Background(), []codesearch.Backend{b}, "needle", codesearch.SearchOptions{}, live)
	}()

	select {
	case <-b.returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the search is still running: it waits for the blocked output")
	}
	select {
	case <-done:
		t.Fatal("runSearchesLive returned before the results were printed")
	case <-time.After(20 * time.Millisecond):
	}
	close(out.release)
	<-done
	if got, want := out.String(), printedAtTheEnd([]*streamingBackend{&b.streamingBackend}, 0, nil); got != want {
		t.Errorf("got output:\n%s\nwant:\n%s", got, want)
	}
}
