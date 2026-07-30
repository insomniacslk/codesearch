package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/insomniacslk/codesearch/pkg/codesearch"
	"github.com/spf13/viper"
)

// resetConfigState returns the package-level flag and config state to the
// values a fresh process would start with. The config plumbing is built on
// globals and on viper's singleton, so every test has to start from a known
// state.
func resetConfigState(t *testing.T) {
	t.Helper()
	viper.Reset()
	globalConfig = codesearch.Config{}
	configFile = ""
	flagDebug = false
	flagStats = false
	flagLimit = 0
	flagSort = ""
	flagSearchContextBefore = 0
	flagSearchContextAfter = 0
	flagCaseInsensitive = false
	flagSearchInFilenames = false
	flagMatchFilename = ""

	for name := range flagToConfigKey {
		f := rootCmd.PersistentFlags().Lookup(name)
		if f == nil {
			f = searchCmd.PersistentFlags().Lookup(name)
		}
		f.Changed = false
		if err := f.Value.Set(f.DefValue); err != nil {
			t.Fatalf("failed to reset flag %q: %v", name, err)
		}
	}
	bindFlags()
	t.Cleanup(func() { viper.Reset() })
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	return path
}

// setFlag simulates the flag being passed on the command line.
func setFlag(t *testing.T, name, value string) {
	t.Helper()
	f := rootCmd.PersistentFlags().Lookup(name)
	if f == nil {
		f = searchCmd.PersistentFlags().Lookup(name)
	}
	if f == nil {
		t.Fatalf("no such flag: %q", name)
	}
	if err := f.Value.Set(value); err != nil {
		t.Fatalf("failed to set flag %q: %v", name, err)
	}
	f.Changed = true
}

const testConfig = `
default_backends: [local]
backends:
  local:
    type: csearch
    params:
      index_file: /tmp/index
`

// TestConfigFileFlagIsRead covers the regression where -c set the config file
// path but never read, unmarshalled or validated it, leaving an empty config.
func TestConfigFileFlagIsRead(t *testing.T) {
	resetConfigState(t)
	configFile = writeConfig(t, testConfig)

	initConfig()

	cfg := getConfig()
	if got, want := len(cfg.Backends), 1; got != want {
		t.Fatalf("got %d backends, want %d", got, want)
	}
	b, ok := cfg.Backends["local"]
	if !ok {
		t.Fatalf("backend %q not found in %v", "local", cfg.Backends)
	}
	if got, want := string(b.Type), "csearch"; got != want {
		t.Errorf("got backend type %q, want %q", got, want)
	}
	if got, want := len(cfg.DefaultBackends), 1; got != want {
		t.Fatalf("got %d default backends, want %d", got, want)
	}
	if got, want := cfg.DefaultBackends[0], "local"; got != want {
		t.Errorf("got default backend %q, want %q", got, want)
	}
}

func TestConfigFileValuesApplied(t *testing.T) {
	resetConfigState(t)
	configFile = writeConfig(t, testConfig+`
stats: true
defaults:
  limit: 5
  sort: z-a
  before: 2
  after: 3
  case_insensitive: true
  match_filename: "*.go"
`)

	initConfig()

	if !flagStats {
		t.Error("stats: got false, want true")
	}
	if got, want := flagLimit, uint(5); got != want {
		t.Errorf("limit: got %d, want %d", got, want)
	}
	if got, want := flagSort, "z-a"; got != want {
		t.Errorf("sort: got %q, want %q", got, want)
	}
	if got, want := flagSearchContextBefore, 2; got != want {
		t.Errorf("before: got %d, want %d", got, want)
	}
	if got, want := flagSearchContextAfter, 3; got != want {
		t.Errorf("after: got %d, want %d", got, want)
	}
	if !flagCaseInsensitive {
		t.Error("case_insensitive: got false, want true")
	}
	if got, want := flagMatchFilename, "*.go"; got != want {
		t.Errorf("match_filename: got %q, want %q", got, want)
	}
}

func TestCommandLineOverridesConfigFile(t *testing.T) {
	resetConfigState(t)
	configFile = writeConfig(t, testConfig+`
defaults:
  limit: 5
  sort: z-a
  before: 2
`)
	setFlag(t, "limit", "10")
	setFlag(t, "sort", "a-z")

	initConfig()

	if got, want := flagLimit, uint(10); got != want {
		t.Errorf("limit: got %d, want %d", got, want)
	}
	if got, want := flagSort, "a-z"; got != want {
		t.Errorf("sort: got %q, want %q", got, want)
	}
	// not overridden on the command line, so the config file still wins
	if got, want := flagSearchContextBefore, 2; got != want {
		t.Errorf("before: got %d, want %d", got, want)
	}
}

func TestFlagDefaultsWhenAbsentEverywhere(t *testing.T) {
	resetConfigState(t)
	configFile = writeConfig(t, testConfig)

	initConfig()

	if got, want := flagLimit, uint(0); got != want {
		t.Errorf("limit: got %d, want %d", got, want)
	}
	if got, want := flagSort, ""; got != want {
		t.Errorf("sort: got %q, want %q", got, want)
	}
	if flagCaseInsensitive {
		t.Error("case_insensitive: got true, want false")
	}
}

// TestBindFlagsCoversEveryKey guards against a flag being renamed without
// updating flagToConfigKey, which bindFlags would panic on at startup.
func TestBindFlagsCoversEveryKey(t *testing.T) {
	resetConfigState(t)
	for name := range flagToConfigKey {
		if rootCmd.PersistentFlags().Lookup(name) == nil &&
			searchCmd.PersistentFlags().Lookup(name) == nil {
			t.Errorf("flag %q in flagToConfigKey does not exist", name)
		}
	}
}

func contentResult(path string, lineno int) codesearch.Result {
	return codesearch.Result{Backend: "b", Path: path, Line: "needle", Lineno: lineno, Highlight: [2]int{0, 6}}
}

// TestSelectResultsLimitCountsPrinted covers the regression where the limit
// was compared against the raw index, so results discarded by the file name
// filter still consumed the budget.
func TestSelectResultsLimitCountsPrinted(t *testing.T) {
	results := codesearch.Results{
		contentResult("a/skip.js", 1),
		contentResult("b/skip.js", 2),
		contentResult("c/skip.js", 3),
		contentResult("d/want.go", 4),
		contentResult("e/want.go", 5),
		contentResult("f/want.go", 6),
	}
	got := selectResults(results, 2, ".go")
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	for _, r := range got {
		if !strings.HasSuffix(r.Path, ".go") {
			t.Errorf("got %q, want only .go paths", r.Path)
		}
	}
	if got[0].Path != "d/want.go" || got[1].Path != "e/want.go" {
		t.Errorf("got %q and %q, want d/want.go and e/want.go", got[0].Path, got[1].Path)
	}
}

func TestSelectResults(t *testing.T) {
	results := codesearch.Results{
		contentResult("one.go", 1),
		contentResult("two.js", 2),
		contentResult("three.go", 3),
	}
	results = append(results, codesearch.Result{Path: "filename-only.go", IsFilename: true})

	for _, tt := range []struct {
		name          string
		limit         uint
		matchFilename string
		want          []string
	}{
		{"no limit no filter", 0, "", []string{"one.go", "two.js", "three.go"}},
		{"limit only", 2, "", []string{"one.go", "two.js"}},
		{"filter only", 0, ".go", []string{"one.go", "three.go"}},
		{"limit above available", 99, "", []string{"one.go", "two.js", "three.go"}},
		{"filter matches nothing", 0, ".rb", nil},
		{"filter is case insensitive", 0, ".GO", []string{"one.go", "three.go"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, r := range selectResults(results, tt.limit, tt.matchFilename) {
				got = append(got, r.Path)
			}
			if !equalStrings(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSelectResultsSkipsFilenameOnlyMatches checks that results flagged as
// file name matches never appear in content output, where they would have no
// line to highlight.
func TestSelectResultsSkipsFilenameOnlyMatches(t *testing.T) {
	results := codesearch.Results{
		{Path: "a.go", IsFilename: true},
		{Path: "b.go", IsFilename: true},
	}
	if got := selectResults(results, 0, ""); len(got) != 0 {
		t.Errorf("got %d results, want 0", len(got))
	}
}

func TestSelectFilenames(t *testing.T) {
	results := codesearch.Results{
		{Path: "cmd/main.go"},
		{Path: "cmd/main.go"},
		{Path: "pkg/main.go"},
	}
	t.Run("deduplicates and sorts", func(t *testing.T) {
		var got []string
		for _, r := range selectFilenames(results, 0) {
			got = append(got, r.Path)
		}
		if want := []string{"cmd/main.go", "pkg/main.go"}; !equalStrings(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("limit counts unique names", func(t *testing.T) {
		got := selectFilenames(results, 1)
		if len(got) != 1 {
			t.Fatalf("got %d results, want 1", len(got))
		}
		if got[0].Path != "cmd/main.go" {
			t.Errorf("got %q, want cmd/main.go", got[0].Path)
		}
	})
	t.Run("duplicates do not consume the limit", func(t *testing.T) {
		dupes := codesearch.Results{
			{Path: "same.go"}, {Path: "same.go"}, {Path: "same.go"}, {Path: "other.go"},
		}
		got := selectFilenames(dupes, 2)
		if len(got) != 2 {
			t.Fatalf("got %d results, want 2", len(got))
		}
	})
}

// TestSelectFilenamesKeepsNonLiteralMatches covers the regression where -F
// results were re-filtered with strings.Contains against the raw search
// string. csearch matches file names with a regexp, so every hit from a
// pattern containing metacharacters was discarded and -F printed nothing.
func TestSelectFilenamesKeepsNonLiteralMatches(t *testing.T) {
	// what a csearch -F search for `\.go$` legitimately returns
	results := codesearch.Results{
		{Path: "cmd/cs/main.go", IsFilename: true},
		{Path: "pkg/codesearch/github.go", IsFilename: true},
	}
	got := selectFilenames(results, 0)
	if len(got) != len(results) {
		t.Fatalf("got %d results, want %d", len(got), len(results))
	}
	want := []string{"cmd/cs/main.go", "pkg/codesearch/github.go"}
	var paths []string
	for _, r := range got {
		paths = append(paths, r.Path)
	}
	if !equalStrings(paths, want) {
		t.Errorf("got %q, want %q", paths, want)
	}
}

func equalStrings(a, b []string) bool {
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

type fakeBackend struct {
	name    string
	caps    map[codesearch.Capability]bool
	results codesearch.Results
	err     error
	delay   time.Duration
	calls   int32
}

func (f *fakeBackend) New(string, codesearch.BackendParams) (codesearch.Backend, error) {
	return f, nil
}
func (f *fakeBackend) Name() string                          { return f.name }
func (f *fakeBackend) Type() string                          { return "fake" }
func (f *fakeBackend) Supports(c codesearch.Capability) bool { return f.caps[c] }
func (f *fakeBackend) SetLinesBefore(int)                    {}
func (f *fakeBackend) SetLinesAfter(int)                     {}
func (f *fakeBackend) SetCaseInsensitive(bool)               {}
func (f *fakeBackend) SetSearchInFilenames(bool)             {}
func (f *fakeBackend) SetTimeout(time.Duration)              {}
func (f *fakeBackend) Search(ctx context.Context, _ string, _ ...codesearch.Opt) (codesearch.Results, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.results, f.err
}

func TestCheckCapabilities(t *testing.T) {
	full := &fakeBackend{name: "full", caps: map[codesearch.Capability]bool{
		codesearch.CapCaseInsensitive:   true,
		codesearch.CapSearchInFilenames: true,
	}}
	none := &fakeBackend{name: "none", caps: map[codesearch.Capability]bool{}}

	for _, tt := range []struct {
		name     string
		backends []codesearch.Backend
		caps     []codesearch.Capability
		wantErr  bool
		wantMsg  []string
	}{
		{
			name:     "nothing requested",
			backends: []codesearch.Backend{none},
			caps:     nil,
		},
		{
			name:     "supported",
			backends: []codesearch.Backend{full},
			caps:     []codesearch.Capability{codesearch.CapCaseInsensitive},
		},
		{
			name:     "unsupported names the backend and the flag",
			backends: []codesearch.Backend{none},
			caps:     []codesearch.Capability{codesearch.CapCaseInsensitive},
			wantErr:  true,
			wantMsg:  []string{"none", "--case-insensitive/-i"},
		},
		{
			name:     "one unsupported among several",
			backends: []codesearch.Backend{full, none},
			caps:     []codesearch.Capability{codesearch.CapSearchInFilenames},
			wantErr:  true,
			wantMsg:  []string{"none", "--search-in-filenames/-F"},
		},
		{
			name:     "every unsupported combination is listed",
			backends: []codesearch.Backend{none},
			caps: []codesearch.Capability{
				codesearch.CapCaseInsensitive, codesearch.CapSearchInFilenames,
			},
			wantErr: true,
			wantMsg: []string{"--case-insensitive/-i", "--search-in-filenames/-F"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCapabilities(tt.backends, tt.caps)
			if tt.wantErr && err == nil {
				t.Fatal("got nil error, want a failure")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("got error %v, want nil", err)
			}
			for _, want := range tt.wantMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestRequestedCapabilities(t *testing.T) {
	defer func() { flagCaseInsensitive, flagSearchInFilenames = false, false }()

	flagCaseInsensitive, flagSearchInFilenames = false, false
	if got := requestedCapabilities(); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
	flagCaseInsensitive, flagSearchInFilenames = true, true
	if got := requestedCapabilities(); len(got) != 2 {
		t.Errorf("got %v, want two capabilities", got)
	}
}

// TestCapabilityFlagsAreNamed makes sure every capability has a flag name, so
// the error message never has a blank where the flag should be.
func TestCapabilityFlagsAreNamed(t *testing.T) {
	for _, c := range []codesearch.Capability{
		codesearch.CapCaseInsensitive, codesearch.CapSearchInFilenames,
	} {
		if capabilityFlags[c] == "" {
			t.Errorf("capability %q has no flag name", c)
		}
	}
}

// TestRunSearchesKeepsGoingAfterAFailure covers the regression where a single
// backend error called logrus.Fatalf, throwing away results that other
// backends had already returned.
func TestRunSearchesKeepsGoingAfterAFailure(t *testing.T) {
	boom := errors.New("upstream 502")
	backends := []codesearch.Backend{
		&fakeBackend{name: "first", results: codesearch.Results{{Path: "a.go"}}},
		&fakeBackend{name: "broken", err: boom},
		&fakeBackend{name: "last", results: codesearch.Results{{Path: "b.go"}, {Path: "c.go"}}},
	}

	outcomes := runSearches(context.Background(), backends, "needle")

	if len(outcomes) != len(backends) {
		t.Fatalf("got %d outcomes, want %d", len(outcomes), len(backends))
	}
	// every backend was asked, including the ones after the failure
	for _, b := range backends {
		if got := atomic.LoadInt32(&b.(*fakeBackend).calls); got != 1 {
			t.Errorf("backend %q: got %d calls, want 1", b.Name(), got)
		}
	}
	if got, want := len(outcomes[0].results), 1; got != want {
		t.Errorf("first: got %d results, want %d", got, want)
	}
	if !errors.Is(outcomes[1].err, boom) {
		t.Errorf("broken: got error %v, want %v", outcomes[1].err, boom)
	}
	if got, want := len(outcomes[2].results), 2; got != want {
		t.Errorf("last: got %d results, want %d", got, want)
	}
}

func TestFailedSearches(t *testing.T) {
	outcomes := []searchOutcome{
		{backend: &fakeBackend{name: "ok"}},
		{backend: &fakeBackend{name: "bad"}, err: errors.New("boom")},
		{backend: &fakeBackend{name: "worse"}, err: errors.New("kaboom")},
	}
	failed := failedSearches(outcomes)
	if got, want := len(failed), 2; got != want {
		t.Fatalf("got %d failures, want %d", got, want)
	}
	if failed[0].backend.Name() != "bad" || failed[1].backend.Name() != "worse" {
		t.Errorf("got %q and %q, want bad and worse",
			failed[0].backend.Name(), failed[1].backend.Name())
	}

	if got := failedSearches(outcomes[:1]); len(got) != 0 {
		t.Errorf("got %d failures for an all-clear run, want 0", len(got))
	}
	if got := failedSearches(nil); len(got) != 0 {
		t.Errorf("got %d failures for no outcomes, want 0", len(got))
	}
}

func TestRunSearchesRecordsDuration(t *testing.T) {
	backends := []codesearch.Backend{&fakeBackend{name: "slow", delay: 20 * time.Millisecond}}
	outcomes := runSearches(context.Background(), backends, "needle")
	if outcomes[0].duration < 20*time.Millisecond {
		t.Errorf("got duration %s, want at least 20ms", outcomes[0].duration)
	}
}

// TestSplitHighlight checks that a highlight range reported by a backend can
// never crash the printer, however malformed it is.
func TestSplitHighlight(t *testing.T) {
	for _, tt := range []struct {
		name              string
		line              string
		h                 [2]int
		head, match, tail string
	}{
		{"normal", "func needle() {}", [2]int{5, 11}, "func ", "needle", "() {}"},
		{"start of line", "needle here", [2]int{0, 6}, "", "needle", " here"},
		{"end of line", "here needle", [2]int{5, 11}, "here ", "needle", ""},
		{"whole line", "needle", [2]int{0, 6}, "", "needle", ""},
		{"empty range", "needle", [2]int{3, 3}, "nee", "", "dle"},
		{"negative start", "needle", [2]int{-1, 3}, "", "nee", "dle"},
		{"end past line", "needle", [2]int{3, 99}, "nee", "dle", ""},
		{"start past line", "needle", [2]int{99, 120}, "needle", "", ""},
		{"inverted range", "needle", [2]int{4, 2}, "need", "", "le"},
		{"empty line", "", [2]int{0, 5}, "", "", ""},
		{"both negative", "needle", [2]int{-5, -2}, "", "", "needle"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			head, match, tail := splitHighlight(tt.line, tt.h)
			if head != tt.head || match != tt.match || tail != tt.tail {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)",
					head, match, tail, tt.head, tt.match, tt.tail)
			}
			// the three pieces must always reassemble into the original line
			if got := head + match + tail; got != tt.line {
				t.Errorf("pieces rejoin to %q, want %q", got, tt.line)
			}
		})
	}
}

// TestRunSearchesIsConcurrent checks that backends are queried in parallel:
// run sequentially, four 100ms backends would take at least 400ms.
func TestRunSearchesIsConcurrent(t *testing.T) {
	const (
		delay = 100 * time.Millisecond
		n     = 4
	)
	backends := make([]codesearch.Backend, 0, n)
	for i := 0; i < n; i++ {
		backends = append(backends, &fakeBackend{name: fmt.Sprintf("b%d", i), delay: delay})
	}

	start := time.Now()
	outcomes := runSearches(context.Background(), backends, "needle")
	elapsed := time.Since(start)

	if len(outcomes) != n {
		t.Fatalf("got %d outcomes, want %d", len(outcomes), n)
	}
	if elapsed >= n*delay {
		t.Errorf("took %s for %d backends of %s each, want them to overlap", elapsed, n, delay)
	}
}

// TestRunSearchesPreservesOrder checks that results come back in the order the
// backends were configured, regardless of which one finishes first.
func TestRunSearchesPreservesOrder(t *testing.T) {
	backends := []codesearch.Backend{
		&fakeBackend{name: "slowest", delay: 60 * time.Millisecond, results: codesearch.Results{{Path: "a"}}},
		&fakeBackend{name: "middle", delay: 30 * time.Millisecond, results: codesearch.Results{{Path: "b"}}},
		&fakeBackend{name: "fastest", results: codesearch.Results{{Path: "c"}}},
	}

	for i := 0; i < 5; i++ {
		outcomes := runSearches(context.Background(), backends, "needle")
		var got []string
		for _, o := range outcomes {
			got = append(got, o.backend.Name())
		}
		if want := []string{"slowest", "middle", "fastest"}; !equalStrings(got, want) {
			t.Fatalf("run %d: got %q, want %q", i, got, want)
		}
		if outcomes[0].results[0].Path != "a" || outcomes[2].results[0].Path != "c" {
			t.Fatalf("run %d: results paired with the wrong backend", i)
		}
	}
}

// TestRunSearchesConcurrentFailures checks that a failing backend does not
// disturb the others now that they run at the same time.
func TestRunSearchesConcurrentFailures(t *testing.T) {
	backends := []codesearch.Backend{
		&fakeBackend{name: "ok1", delay: 20 * time.Millisecond, results: codesearch.Results{{Path: "a"}}},
		&fakeBackend{name: "bad", err: errors.New("boom")},
		&fakeBackend{name: "ok2", delay: 10 * time.Millisecond, results: codesearch.Results{{Path: "b"}}},
	}
	outcomes := runSearches(context.Background(), backends, "needle")

	if got := failedSearches(outcomes); len(got) != 1 {
		t.Fatalf("got %d failures, want 1", len(got))
	}
	for _, i := range []int{0, 2} {
		if outcomes[i].err != nil {
			t.Errorf("outcome %d: got error %v, want nil", i, outcomes[i].err)
		}
		if len(outcomes[i].results) != 1 {
			t.Errorf("outcome %d: got %d results, want 1", i, len(outcomes[i].results))
		}
	}
}

// TestRunSearchesHonoursCancellation checks that cancelling the run stops the
// backends instead of waiting for every one of them to finish.
func TestRunSearchesHonoursCancellation(t *testing.T) {
	backends := []codesearch.Backend{
		&fakeBackend{name: "slow1", delay: 10 * time.Second},
		&fakeBackend{name: "slow2", delay: 10 * time.Second},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	outcomes := runSearches(ctx, backends, "needle")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s, want it to stop when cancelled", elapsed)
	}
	for _, o := range outcomes {
		if !errors.Is(o.err, context.Canceled) {
			t.Errorf("backend %q: got error %v, want context.Canceled", o.backend.Name(), o.err)
		}
	}
}

func TestWriteJSON(t *testing.T) {
	outcomes := []searchOutcome{
		{
			backend:  &fakeBackend{name: "local"},
			duration: 150 * time.Millisecond,
			selected: []*codesearch.Result{
				{
					Backend: "local", Path: "cmd/cs/main.go", Line: "func main() {}",
					Lineno: 42, Highlight: [2]int{5, 9}, FileURL: "file:///cmd/cs/main.go",
					Context: codesearch.ResultContext{Before: []string{"prev"}, After: []string{"next"}},
				},
			},
		},
		{
			backend:  &fakeBackend{name: "broken"},
			duration: 20 * time.Millisecond,
			err:      errors.New("upstream 502"),
		},
	}

	var buf bytes.Buffer
	if err := writeJSON(&buf, outcomes, 1, 200*time.Millisecond); err != nil {
		t.Fatalf("writeJSON failed: %v", err)
	}

	var got jsonOutput
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if got.TotalResults != 1 {
		t.Errorf("total_results: got %d, want 1", got.TotalResults)
	}
	if got.DurationMS != 200 {
		t.Errorf("duration_ms: got %d, want 200", got.DurationMS)
	}
	if len(got.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(got.Results))
	}
	r := got.Results[0]
	if r.Path != "cmd/cs/main.go" || r.Lineno != 42 || r.Line != "func main() {}" {
		t.Errorf("result round-tripped as %+v", r)
	}
	if len(r.Context.Before) != 1 || r.Context.Before[0] != "prev" {
		t.Errorf("context did not round-trip: %+v", r.Context)
	}

	if len(got.Backends) != 2 {
		t.Fatalf("got %d backends, want 2", len(got.Backends))
	}
	// a failing backend must be visible, so an empty result set can be told
	// apart from a broken one
	if got.Backends[1].Error == "" {
		t.Error("failing backend reported no error")
	}
	if got.Backends[0].Error != "" {
		t.Errorf("healthy backend reported error %q", got.Backends[0].Error)
	}
	if got.Backends[0].Results != 1 || got.Backends[1].Results != 0 {
		t.Errorf("per-backend counts wrong: %+v", got.Backends)
	}
}

// TestWriteJSONEmptyIsAnArray keeps the results field an array rather than
// null, so consumers can iterate it unconditionally.
func TestWriteJSONEmptyIsAnArray(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, nil, 0, 0); err != nil {
		t.Fatalf("writeJSON failed: %v", err)
	}
	if !strings.Contains(buf.String(), `"results": []`) {
		t.Errorf("got %s, want an empty results array", buf.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
}
