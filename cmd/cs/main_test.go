package main

import (
	"errors"
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
func (f *fakeBackend) Search(string, ...codesearch.Opt) (codesearch.Results, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		time.Sleep(f.delay)
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

	outcomes := runSearches(backends, "needle")

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
	outcomes := runSearches(backends, "needle")
	if outcomes[0].duration < 20*time.Millisecond {
		t.Errorf("got duration %s, want at least 20ms", outcomes[0].duration)
	}
}
