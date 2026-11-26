package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fatih/color"
	"github.com/insomniacslk/codesearch/pkg/codesearch"
	"github.com/kirsle/configdir"
	"github.com/sirupsen/logrus"
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
	flagSearchContext = 0
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

// useConfigDir points the config search path at dir. Both variables are set
// because configdir picks a different one per platform: XDG_CONFIG_HOME
// everywhere except macOS and Windows, and $HOME/Library/Application Support on
// macOS. It reads them once in its own init, so it has to be told to reread.
func useConfigDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	configdir.Refresh()
	t.Cleanup(configdir.Refresh)

	// whichever rule applies, the search path has to land inside dir, or the
	// test is silently reading the real user's configuration
	if got := configPath(); !strings.HasPrefix(got, dir) {
		t.Fatalf("config path %q is outside the test directory %q", got, dir)
	}
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

	if err := loadConfig(); err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

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

	if err := loadConfig(); err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

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

	if err := loadConfig(); err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

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

	if err := loadConfig(); err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

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
	got := selectResults(results, 2, mustPattern(t, ".go"))
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
		{"filter only", 0, `\.go`, []string{"one.go", "three.go"}},
		{"limit above available", 99, "", []string{"one.go", "two.js", "three.go"}},
		{"filter matches nothing", 0, `\.rb`, nil},
		{"filter is case insensitive", 0, `\.GO`, []string{"one.go", "three.go"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, r := range selectResults(results, tt.limit, mustPattern(t, tt.matchFilename)) {
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
	if got := selectResults(results, 0, nil); len(got) != 0 {
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
		for _, r := range selectFilenames(results, 0, false) {
			got = append(got, r.Path)
		}
		if want := []string{"cmd/main.go", "pkg/main.go"}; !equalStrings(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("limit counts unique names", func(t *testing.T) {
		got := selectFilenames(results, 1, false)
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
		got := selectFilenames(dupes, 2, false)
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
	got := selectFilenames(results, 0, false)
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
func (f *fakeBackend) Search(ctx context.Context, _ string, _ codesearch.SearchOptions) (codesearch.Results, error) {
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

	outcomes := runSearches(context.Background(), backends, "needle", codesearch.SearchOptions{})

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
	outcomes := runSearches(context.Background(), backends, "needle", codesearch.SearchOptions{})
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
	// long enough that the gap between concurrent and sequential survives a
	// slow or loaded CI runner
	const (
		delay = 250 * time.Millisecond
		n     = 4
	)
	backends := make([]codesearch.Backend, 0, n)
	for i := 0; i < n; i++ {
		backends = append(backends, &fakeBackend{name: fmt.Sprintf("b%d", i), delay: delay})
	}

	start := time.Now()
	outcomes := runSearches(context.Background(), backends, "needle", codesearch.SearchOptions{})
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
		outcomes := runSearches(context.Background(), backends, "needle", codesearch.SearchOptions{})
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
	outcomes := runSearches(context.Background(), backends, "needle", codesearch.SearchOptions{})

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
	outcomes := runSearches(ctx, backends, "needle", codesearch.SearchOptions{})
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

func TestToAnsiURL(t *testing.T) {
	old := color.NoColor
	defer func() { color.NoColor = old }()

	t.Run("plain text when colour is off", func(t *testing.T) {
		color.NoColor = true
		got := toAnsiURL("https://example.com/f.go", "f.go")
		if got != "f.go" {
			t.Errorf("got %q, want %q", got, "f.go")
		}
		if strings.Contains(got, "\033") {
			t.Errorf("got %q, want no escape sequences", got)
		}
	})

	t.Run("hyperlink when colour is on", func(t *testing.T) {
		color.NoColor = false
		got := toAnsiURL("https://example.com/f.go", "f.go")
		if !strings.Contains(got, "https://example.com/f.go") {
			t.Errorf("got %q, want it to carry the URL", got)
		}
		if !strings.Contains(got, "\033]8;;") {
			t.Errorf("got %q, want an OSC 8 hyperlink", got)
		}
		if !strings.Contains(got, "f.go") {
			t.Errorf("got %q, want it to carry the text", got)
		}
	})
}

// TestPrintedOutputIsPlainWhenColourIsOff is the point of gating hyperlinks:
// redirected output must not have terminal escapes embedded in it.
func TestPrintedOutputIsPlainWhenColourIsOff(t *testing.T) {
	old := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = old }()

	var buf bytes.Buffer
	printResult(&buf, &codesearch.Result{
		Backend: "local", Path: "cmd/cs/main.go", Line: "func main() {}",
		Lineno: 42, Highlight: [2]int{5, 9},
		RepoURL: "file:///repo", FileURL: "file:///repo/cmd/cs/main.go",
		Context: codesearch.ResultContext{Before: []string{"prev"}, After: []string{"next"}},
	}, false)

	out := buf.String()
	if strings.Contains(out, "\033") {
		t.Errorf("output contains escape sequences:\n%q", out)
	}
	for _, want := range []string{"cmd/cs/main.go", "func main() {}", "42", "prev", "next"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

// TestContextBlockNumbering checks that the line numbers printed beside the
// context lines are the ones those lines actually have in the file.
func TestContextBlockNumbering(t *testing.T) {
	res := &codesearch.Result{
		Lineno: 10,
		Context: codesearch.ResultContext{
			Before: []string{"seven", "eight", "nine"},
			After:  []string{"eleven", "twelve"},
		},
	}
	before, after := contextBlock(res)
	if want := "7: seven\n8: eight\n9: nine\n"; before != want {
		t.Errorf("before:\ngot  %q\nwant %q", before, want)
	}
	if want := "\n11: eleven\n12: twelve\n"; after != want {
		t.Errorf("after:\ngot  %q\nwant %q", after, want)
	}
}

// TestContextBlockUnknownLineno covers a result whose line in the file is not
// known: numbering from 0 would show the lines above the match as negative.
func TestContextBlockUnknownLineno(t *testing.T) {
	res := &codesearch.Result{
		Context: codesearch.ResultContext{
			Before: []string{"one", "two"},
			After:  []string{"four"},
		},
	}
	before, after := contextBlock(res)
	if want := "one\ntwo\n"; before != want {
		t.Errorf("before:\ngot  %q\nwant %q", before, want)
	}
	if want := "\nfour\n"; after != want {
		t.Errorf("after:\ngot  %q\nwant %q", after, want)
	}
}

func TestPrintResultUnknownLineno(t *testing.T) {
	old := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = old }()

	for _, tt := range []struct {
		lineno int
		want   string
	}{
		{0, "gh:o/r:f.go (main)\n\nabove\nthe needle\nbelow\n\n\n"},
		{5, "gh:o/r:f.go (main)\n\n4: above\n5: the needle\n6: below\n\n\n"},
	} {
		var buf bytes.Buffer
		printResult(&buf, &codesearch.Result{
			Backend: "gh", Owner: "o", RepoName: "r", Path: "f.go", Branch: "main",
			Line: "the needle", Lineno: tt.lineno, Highlight: [2]int{4, 10},
			Context: codesearch.ResultContext{Before: []string{"above"}, After: []string{"below"}},
		}, false)
		if got := buf.String(); got != tt.want {
			t.Errorf("lineno %d:\ngot  %q\nwant %q", tt.lineno, got, tt.want)
		}
	}
}

func TestContextBlockEmpty(t *testing.T) {
	before, after := contextBlock(&codesearch.Result{Lineno: 3})
	if before != "" || after != "" {
		t.Errorf("got (%q, %q), want both empty", before, after)
	}
}

// TestPrintResultFilenamesOnly checks the -F rendering prints the location and
// nothing else, since there is no matching line to show.
func TestPrintResultFilenamesOnly(t *testing.T) {
	old := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = old }()

	var buf bytes.Buffer
	printResult(&buf, &codesearch.Result{
		Backend: "local", Path: "cmd/cs/main.go", Branch: "main",
		RepoURL: "file:///repo", FileURL: "file:///repo/cmd/cs/main.go",
		IsFilename: true,
	}, true)

	if got, want := buf.String(), "local::cmd/cs/main.go (main)\n\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func mustPattern(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := compileFilenamePattern(pattern)
	if err != nil {
		t.Fatalf("failed to compile %q: %v", pattern, err)
	}
	return re
}

func TestCompileFilenamePattern(t *testing.T) {
	results := codesearch.Results{
		contentResult("cmd/cs/main.go", 1),
		contentResult("pkg/codesearch/github.go", 2),
		contentResult("README.md", 3),
		contentResult("docs/GO.md", 4),
	}
	for _, tt := range []struct {
		name, pattern string
		want          []string
	}{
		{"empty matches everything", "", []string{"cmd/cs/main.go", "pkg/codesearch/github.go", "README.md", "docs/GO.md"}},
		{"substring still works", ".go", []string{"cmd/cs/main.go", "pkg/codesearch/github.go", "docs/GO.md"}},
		{"anchored extension", `\.go$`, []string{"cmd/cs/main.go", "pkg/codesearch/github.go"}},
		{"anchored prefix", "^cmd/", []string{"cmd/cs/main.go"}},
		{"alternation", `\.(md)$`, []string{"README.md", "docs/GO.md"}},
		{"character class", `^[dp]`, []string{"pkg/codesearch/github.go", "docs/GO.md"}},
		{"case insensitive by default", `readme`, []string{"README.md"}},
		{"case sensitivity can be requested", `(?-i)readme`, nil},
		{"matches nothing", `\.rb$`, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, r := range selectResults(results, 0, mustPattern(t, tt.pattern)) {
				got = append(got, r.Path)
			}
			if !equalStrings(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompileFilenamePatternRejectsBadRegexp(t *testing.T) {
	re, err := compileFilenamePattern("[unclosed")
	if err == nil {
		t.Fatalf("got %v, want an error for an invalid regexp", re)
	}
	if !strings.Contains(err.Error(), "[unclosed") {
		t.Errorf("error %q does not quote the pattern", err)
	}
}

func TestCompileFilenamePatternEmptyIsNil(t *testing.T) {
	re, err := compileFilenamePattern("")
	if err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	if re != nil {
		t.Errorf("got %v, want nil for an empty pattern", re)
	}
}

func TestContextFlag(t *testing.T) {
	for _, tt := range []struct {
		name                  string
		config                string
		flags                 map[string]string
		wantBefore, wantAfter int
	}{
		{
			name:       "context sets both sides",
			flags:      map[string]string{"context": "3"},
			wantBefore: 3, wantAfter: 3,
		},
		{
			name:       "explicit before wins",
			flags:      map[string]string{"context": "3", "before": "1"},
			wantBefore: 1, wantAfter: 3,
		},
		{
			name:       "explicit after wins",
			flags:      map[string]string{"context": "3", "after": "5"},
			wantBefore: 3, wantAfter: 5,
		},
		{
			name:       "both explicit",
			flags:      map[string]string{"context": "3", "before": "1", "after": "2"},
			wantBefore: 1, wantAfter: 2,
		},
		{
			name:       "no context flag leaves the sides alone",
			flags:      map[string]string{"before": "2"},
			wantBefore: 2, wantAfter: 0,
		},
		{
			name:       "context from the config file",
			config:     "defaults:\n  context: 4\n",
			wantBefore: 4, wantAfter: 4,
		},
		{
			name:       "command line context overrides the config file",
			config:     "defaults:\n  context: 4\n",
			flags:      map[string]string{"context": "1"},
			wantBefore: 1, wantAfter: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetConfigState(t)
			configFile = writeConfig(t, testConfig+tt.config)
			for name, value := range tt.flags {
				setFlag(t, name, value)
			}

			if err := loadConfig(); err != nil {
				t.Fatalf("failed to load config: %v", err)
			}

			if flagSearchContextBefore != tt.wantBefore {
				t.Errorf("before: got %d, want %d", flagSearchContextBefore, tt.wantBefore)
			}
			if flagSearchContextAfter != tt.wantAfter {
				t.Errorf("after: got %d, want %d", flagSearchContextAfter, tt.wantAfter)
			}
		})
	}
}

// runCommand executes the CLI in-process with the given arguments. stdout and
// stderr are captured separately, because which stream something lands on is
// part of the contract: config-example has to be redirectable into a file.
func runCommand(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

// TestConfigExampleWithoutConfigFile covers the chicken and egg: config-example
// exists to produce a config file, so it must not require one. The config was
// previously loaded through cobra.OnInitialize, which runs for every command.
func TestConfigExampleWithoutConfigFile(t *testing.T) {
	resetConfigState(t)
	// a config dir that certainly holds no config
	useConfigDir(t, t.TempDir())

	out, errOut, err := runCommand(t, "config-example")
	if err != nil {
		t.Fatalf("got error %v, want nil", err)
	}
	for _, want := range []string{"default_backends", "backends:", "type: github", "index_file"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout does not contain %q", want)
		}
	}
	// it has to be on stdout, or "cs config-example > config.yml" writes an
	// empty file
	if errOut != "" {
		t.Errorf("wrote %q to stderr, want everything on stdout", errOut)
	}
}

// TestConfigExampleOutputIsUsable checks the printed example is valid YAML that
// the config loader accepts, so following the error message actually works.
func TestConfigExampleOutputIsUsable(t *testing.T) {
	resetConfigState(t)
	useConfigDir(t, t.TempDir())
	out, _, err := runCommand(t, "config-example")
	if err != nil {
		t.Fatalf("got error %v, want nil", err)
	}

	resetConfigState(t)
	configFile = writeConfig(t, out)
	if err := loadConfig(); err != nil {
		t.Fatalf("the example config does not load: %v", err)
	}
	if len(getConfig().Backends) == 0 {
		t.Error("the example config declares no backends")
	}
}

// TestCommandsNeedingConfigReportItHelpfully checks that the commands that do
// need a config say where they looked and how to create one.
func TestCommandsNeedingConfigReportItHelpfully(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"search", "needle"}} {
		t.Run(args[0], func(t *testing.T) {
			resetConfigState(t)
			dir := t.TempDir()
			useConfigDir(t, dir)

			_, _, err := runCommand(t, args...)
			if err == nil {
				t.Fatal("got nil error, want a failure")
			}
			msg := err.Error()
			if !strings.Contains(msg, dir) {
				t.Errorf("error %q does not say where it looked", msg)
			}
			if !strings.Contains(msg, "config-example") {
				t.Errorf("error %q does not say how to create a config", msg)
			}
		})
	}
}

func TestLoadConfigErrorsAreDescriptive(t *testing.T) {
	t.Run("malformed yaml", func(t *testing.T) {
		resetConfigState(t)
		configFile = writeConfig(t, "default_backends: [oops\n  bad indent:\n")
		err := loadConfig()
		if err == nil {
			t.Fatal("got nil error, want a failure")
		}
		if !strings.Contains(err.Error(), configFile) {
			t.Errorf("error %q does not name the file", err)
		}
	})
	t.Run("invalid config", func(t *testing.T) {
		resetConfigState(t)
		configFile = writeConfig(t, "default_backends: [nosuchbackend]\nbackends: {}\n")
		err := loadConfig()
		if err == nil {
			t.Fatal("got nil error, want a failure")
		}
		if !strings.Contains(err.Error(), "nosuchbackend") {
			t.Errorf("error %q does not name the offending backend", err)
		}
	})
}

// TestBackendLimitOnlyPushedDownWhenSafe checks that the limit reaches the
// backend only when nothing downstream reorders or discards results. Pushing it
// down under --sort would sort an arbitrary prefix instead of the whole set,
// and under -f it could return fewer results than asked for.
func TestBackendLimitOnlyPushedDownWhenSafe(t *testing.T) {
	for _, tt := range []struct {
		name              string
		limit             uint
		sort              string
		matchFilename     string
		searchInFilenames bool
		want              uint
	}{
		{name: "plain search", limit: 10, want: 10},
		{name: "no limit set", limit: 0, want: 0},
		{name: "sorting needs every result", limit: 10, sort: "a-z", want: 0},
		{name: "filter may reject anything", limit: 10, matchFilename: `\.go$`, want: 0},
		{name: "-F dedupes and re-sorts", limit: 10, searchInFilenames: true, want: 0},
		{name: "sort and filter together", limit: 10, sort: "z-a", matchFilename: "x", want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				flagLimit, flagSort, flagMatchFilename, flagSearchInFilenames = 0, "", "", false
			}()
			flagLimit = tt.limit
			flagSort = tt.sort
			flagMatchFilename = tt.matchFilename
			flagSearchInFilenames = tt.searchInFilenames

			if got := backendLimit(); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// TestLimitedSearchStillHonoursThePrintedLimit checks the two limits agree: the
// backend may return more than asked for, and the printer still trims to it.
func TestLimitedSearchStillHonoursThePrintedLimit(t *testing.T) {
	results := codesearch.Results{
		contentResult("a.go", 1), contentResult("b.go", 2), contentResult("c.go", 3),
	}
	if got := selectResults(results, 2, nil); len(got) != 2 {
		t.Errorf("got %d results, want 2", len(got))
	}
}

// TestMixedCaseBackendNames covers backend names with upper case letters.
// Viper lowercases every map key, so the name under `backends` no longer
// matched the same name written in default_backends or passed to -b.
func TestMixedCaseBackendNames(t *testing.T) {
	resetConfigState(t)
	configFile = writeConfig(t, `
default_backends: [GitHub_Me]
backends:
  GitHub_Me:
    type: csearch
    params:
      index_file: /tmp/index
`)
	if err := loadConfig(); err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	for _, requested := range []string{"GitHub_Me", "github_me", "GITHUB_ME"} {
		names, err := resolveBackendNames(getConfig(), []string{requested})
		if err != nil {
			t.Errorf("%q: got error %v, want nil", requested, err)
			continue
		}
		if len(names) != 1 {
			t.Errorf("%q: got %q, want one backend", requested, names)
			continue
		}
		if _, ok := getConfig().Backends[names[0]]; !ok {
			t.Errorf("%q: resolved to %q, which is not a key of the config", requested, names[0])
		}
	}
}

func backendsConfig(names ...string) *codesearch.Config {
	cfg := &codesearch.Config{Backends: map[string]codesearch.BackendConfig{}}
	for _, n := range names {
		cfg.Backends[n] = codesearch.BackendConfig{Type: codesearch.BackendTypeCsearch}
	}
	return cfg
}

// TestResolveBackendNames covers "all" expanding in map iteration order, which
// changed the order of the output from one run to the next, and -b values that
// carried spaces or repeated a backend.
func TestResolveBackendNames(t *testing.T) {
	cfg := backendsConfig("delta", "alpha", "charlie", "bravo", "echo", "foxtrot", "golf", "hotel")
	all := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	for _, tt := range []struct {
		name      string
		requested []string
		want      []string
	}{
		{"all is sorted", []string{"all"}, all},
		{"all wins over other names", []string{"echo", "all"}, all},
		{"all is case-insensitive", []string{"ALL"}, all},
		{"explicit order is kept", []string{"echo", "alpha"}, []string{"echo", "alpha"}},
		{"whitespace is trimmed", []string{" echo", "alpha "}, []string{"echo", "alpha"}},
		{"empty entries are dropped", []string{"echo", "", " ", "alpha"}, []string{"echo", "alpha"}},
		{"duplicates keep the first", []string{"echo", "alpha", "echo"}, []string{"echo", "alpha"}},
		{"duplicates differing in case", []string{"echo", "ECHO"}, []string{"echo"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// map iteration order varies, so one lucky run proves nothing
			for i := 0; i < 20; i++ {
				got, err := resolveBackendNames(cfg, tt.requested)
				if err != nil {
					t.Fatalf("got error %v, want nil", err)
				}
				if !equalStrings(got, tt.want) {
					t.Fatalf("got %q, want %q", got, tt.want)
				}
			}
		})
	}
}

func TestResolveBackendNamesUnknown(t *testing.T) {
	_, err := resolveBackendNames(backendsConfig("alpha"), []string{"alpha", "nope"})
	if err == nil {
		t.Fatal("got nil error, want a failure for an unknown backend")
	}
	if !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("error %q does not name the backend", err)
	}
}

// TestSelectFilenamesAcrossRoots covers the regression where -F deduplicated on
// the path alone: a csearch index with two roots that both hold README.md
// printed only one of them.
func TestSelectFilenamesAcrossRoots(t *testing.T) {
	results := codesearch.Results{
		{Path: "README.md", RepoURL: "file:///b"},
		{Path: "README.md", RepoURL: "file:///a"},
		{Path: "README.md", RepoURL: "file:///b"},
	}
	got := selectFilenames(results, 0, false)
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	// same path, so the repository breaks the tie
	if got[0].RepoURL != "file:///a" || got[1].RepoURL != "file:///b" {
		t.Errorf("got %q then %q, want file:///a then file:///b", got[0].RepoURL, got[1].RepoURL)
	}
}

// TestSelectFilenamesOrderAndLimit covers -F ignoring --sort z-a, and the limit
// being applied before the sort, which kept an arbitrary subset rather than
// the first names in the printed order.
func TestSelectFilenamesOrderAndLimit(t *testing.T) {
	results := codesearch.Results{
		{Path: "c.go"}, {Path: "a.go"}, {Path: "d.go"}, {Path: "b.go"},
	}
	paths := func(rs []*codesearch.Result) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Path)
		}
		return out
	}
	t.Run("limit applies after the default order", func(t *testing.T) {
		got := paths(selectFilenames(append(codesearch.Results{}, results...), 2, false))
		if want := []string{"a.go", "b.go"}; !equalStrings(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("sort z-a is kept", func(t *testing.T) {
		sorted := getSorter("z-a")(append(codesearch.Results{}, results...))
		got := paths(selectFilenames(sorted, 0, true))
		if want := []string{"d.go", "c.go", "b.go", "a.go"}; !equalStrings(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("sort z-a with a limit", func(t *testing.T) {
		sorted := getSorter("z-a")(append(codesearch.Results{}, results...))
		got := paths(selectFilenames(sorted, 2, true))
		if want := []string{"d.go", "c.go"}; !equalStrings(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// TestNegativeValuesRejected covers negative context and limit values. The
// context values reached the backends, and a negative limit wrapped round to a
// huge unsigned number. A value that is not a number at all used to become 0
// without a word.
func TestNegativeValuesRejected(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config string
		want   string
	}{
		{"defaults.before", "defaults:\n  before: -1\n", "defaults.before"},
		{"defaults.after", "defaults:\n  after: -1\n", "defaults.after"},
		{"defaults.context", "defaults:\n  context: -1\n", "defaults.context"},
		{"defaults.limit", "defaults:\n  limit: -1\n", "defaults.limit"},
		{"not a number", "defaults:\n  before: abc\n", "defaults.before"},
		{"too large", "defaults:\n  after: 4294967296\n", "defaults.after"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetConfigState(t)
			configFile = writeConfig(t, testConfig+tt.config)
			err := loadConfig()
			if err == nil {
				t.Fatalf("got nil error, want a failure (limit=%d before=%d after=%d)",
					flagLimit, flagSearchContextBefore, flagSearchContextAfter)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %s", err, tt.want)
			}
		})
	}
}

// TestNegativeFlagsRejectedByPflag checks that the command line never gets as
// far as the config: the flags are unsigned, so parsing refuses the value.
func TestNegativeFlagsRejectedByPflag(t *testing.T) {
	for _, name := range []string{"before", "after", "context", "limit"} {
		t.Run(name, func(t *testing.T) {
			f := searchCmd.PersistentFlags().Lookup(name)
			if f == nil {
				t.Fatalf("no such flag: %q", name)
			}
			if err := f.Value.Set("-1"); err == nil {
				_ = f.Value.Set(f.DefValue)
				t.Errorf("--%s accepted -1", name)
			}
		})
	}
}

// TestEnvironmentVariables covers AutomaticEnv without a prefix: an unrelated
// DEBUG or STATS in the shell switched cs options on, while the nested keys
// could not be reached from the environment at all.
func TestEnvironmentVariables(t *testing.T) {
	level := logrus.GetLevel()
	t.Cleanup(func() { logrus.SetLevel(level) })

	t.Run("bare names are ignored", func(t *testing.T) {
		resetConfigState(t)
		t.Setenv("DEBUG", "1")
		t.Setenv("STATS", "1")
		t.Setenv("DEFAULTS_LIMIT", "3")
		configFile = writeConfig(t, testConfig)
		if err := loadConfig(); err != nil {
			t.Fatalf("failed to load config: %v", err)
		}
		if flagDebug || flagStats || flagLimit != 0 {
			t.Errorf("got debug=%v stats=%v limit=%d, want them untouched", flagDebug, flagStats, flagLimit)
		}
	})
	t.Run("CS_ names apply", func(t *testing.T) {
		resetConfigState(t)
		t.Setenv("CS_DEBUG", "1")
		t.Setenv("CS_STATS", "true")
		t.Setenv("CS_DEFAULTS_LIMIT", "7")
		t.Setenv("CS_DEFAULTS_SORT", "z-a")
		configFile = writeConfig(t, testConfig)
		if err := loadConfig(); err != nil {
			t.Fatalf("failed to load config: %v", err)
		}
		if !flagDebug || !flagStats || flagLimit != 7 || flagSort != "z-a" {
			t.Errorf("got debug=%v stats=%v limit=%d sort=%q, want true true 7 z-a",
				flagDebug, flagStats, flagLimit, flagSort)
		}
	})
	t.Run("environment overrides the config file", func(t *testing.T) {
		resetConfigState(t)
		t.Setenv("CS_DEFAULTS_LIMIT", "7")
		configFile = writeConfig(t, testConfig+"defaults:\n  limit: 5\n")
		if err := loadConfig(); err != nil {
			t.Fatalf("failed to load config: %v", err)
		}
		if flagLimit != 7 {
			t.Errorf("got limit %d, want 7", flagLimit)
		}
	})
	t.Run("command line overrides the environment", func(t *testing.T) {
		resetConfigState(t)
		t.Setenv("CS_DEFAULTS_LIMIT", "7")
		configFile = writeConfig(t, testConfig+"defaults:\n  limit: 5\n")
		setFlag(t, "limit", "10")
		if err := loadConfig(); err != nil {
			t.Fatalf("failed to load config: %v", err)
		}
		if flagLimit != 10 {
			t.Errorf("got limit %d, want 10", flagLimit)
		}
	})
	t.Run("a negative value from the environment is rejected", func(t *testing.T) {
		resetConfigState(t)
		t.Setenv("CS_DEFAULTS_LIMIT", "-1")
		configFile = writeConfig(t, testConfig)
		if err := loadConfig(); err == nil {
			t.Errorf("got nil error, want a failure (limit=%d)", flagLimit)
		}
	})
}

// TestNoColorEnvironment checks that NO_COLOR still disables colours once
// viper no longer maps it to no_color. fatih/color reads it only while the
// package initialises, so the check needs a fresh process.
func TestNoColorEnvironment(t *testing.T) {
	if os.Getenv("CS_TEST_NO_COLOR_CHILD") == "1" {
		// stdout is not a terminal under go test, which already turns colours
		// off; undo that so only NO_COLOR is left to do it
		color.NoColor = false
		resetConfigState(t)
		configFile = writeConfig(t, testConfig)
		if err := loadConfig(); err != nil {
			t.Fatalf("failed to load config: %v", err)
		}
		fmt.Printf("bold=%q red=%q\n", textBold.Sprint("x"), textBoldRed.Sprint("x"))
		return
	}
	for _, tt := range []struct {
		noColor   string
		wantPlain bool
	}{
		{"1", true},
		{"", false},
	} {
		t.Run(fmt.Sprintf("NO_COLOR=%q", tt.noColor), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNoColorEnvironment$", "-test.v")
			cmd.Env = append(os.Environ(), "CS_TEST_NO_COLOR_CHILD=1", "NO_COLOR="+tt.noColor)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child failed: %v\n%s", err, out)
			}
			plain := strings.Contains(string(out), `bold="x" red="x"`)
			if plain != tt.wantPlain {
				t.Errorf("got plain=%v, want %v; output:\n%s", plain, tt.wantPlain, out)
			}
		})
	}
}
