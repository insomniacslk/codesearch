package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/insomniacslk/codesearch/pkg/codesearch"
	"github.com/kirsle/configdir"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cast"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const progname = "cs"

//go:embed config.yml.example
var configFileExample string

var (
	globalConfig codesearch.Config

	textBold      = color.New(color.Bold)
	textBoldGreen = color.New(color.FgGreen, color.Bold)
	textBoldRed   = color.New(color.FgRed, color.Bold)
)

func getConfig() *codesearch.Config {
	return &globalConfig
}

// settings are the effective values of the command line flags, after the
// config file, the environment and the flag defaults are taken into account.
// They are resolved once per run and passed to whatever needs them.
type settings struct {
	debug   bool
	stats   bool
	noColor bool
	limit   uint
	sort    string
	// linesBefore and linesAfter already include what -C asked for
	linesBefore       int
	linesAfter        int
	caseInsensitive   bool
	searchInFilenames bool
	matchFilename     string
	timeout           time.Duration
	json              bool
	// backends is the raw -b/--backends value; empty means the config's
	// default_backends
	backends string
}

func init() {
	rootCmd.PersistentFlags().StringP("config", "c", "", "Configuration file")
	rootCmd.PersistentFlags().BoolP("debug", "d", false, "Print debug messages")
	rootCmd.PersistentFlags().BoolP("stats", "S", false, "Print stats")
	rootCmd.PersistentFlags().Bool("no-color", false, "Disable colours and terminal hyperlinks. They are disabled automatically when stdout is not a terminal, or when NO_COLOR is set")

	searchCmd.PersistentFlags().StringP("backends", "b", "", "Comma-separated list of names of the backends to use. The names are defined in your configuration file. If specified, it overrides `default_backends` in the configuration file. \"all\" will use every backend")
	searchCmd.PersistentFlags().BoolP("search-in-filenames", "F", false, "Search only in file names")
	searchCmd.PersistentFlags().StringP("match-filename", "f", "", "Show results only from files whose path matches this regexp. Unanchored and case-insensitive, so \".go\" matches anywhere in the path and \"\\.go$\" only at the end")
	searchCmd.PersistentFlags().UintP("before", "B", 0, "Number of context lines to show before the result")
	searchCmd.PersistentFlags().UintP("after", "A", 0, "Number of context lines to show after the result")
	searchCmd.PersistentFlags().UintP("context", "C", 0, "Number of context lines to show on both sides of the result. A non-zero --before or --after takes precedence over it")
	searchCmd.PersistentFlags().BoolP("case-insensitive", "i", false, "Case-insensitive search")
	searchCmd.PersistentFlags().UintP("limit", "l", 0, "Limit the amount of results that are printed per backend. 0 means no limit. Unless --sort, --match-filename or -F is in play, backends stop fetching once the limit is met, which matters on rate-limited APIs")
	searchCmd.PersistentFlags().StringP("sort", "s", "", "Sort the results. Possible values: \"a-z\", \"z-a\"")
	searchCmd.PersistentFlags().DurationP("timeout", "t", 0, "Maximum time to spend on each backend, e.g. \"30s\" or \"2m\". 0 means no limit")
	searchCmd.PersistentFlags().Bool("json", false, "Print results as JSON on stdout instead of formatted text")

	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(configExampleCmd)

	bindFlags()
}

// flagToConfigKey maps command line flag names to configuration keys. Viper
// resolves a bound key to the command line value if the flag was set, then to
// the config file value, and finally to the flag default, which gives the
// command line precedence over the config file.
var flagToConfigKey = map[string]string{
	"debug":               "debug",
	"stats":               "stats",
	"no-color":            "no_color",
	"limit":               "defaults.limit",
	"sort":                "defaults.sort",
	"before":              "defaults.before",
	"after":               "defaults.after",
	"context":             "defaults.context",
	"case-insensitive":    "defaults.case_insensitive",
	"search-in-filenames": "defaults.search_in_filenames",
	"match-filename":      "defaults.match_filename",
	"timeout":             "defaults.timeout",
	"json":                "defaults.json",
}

func bindFlags() {
	for name, key := range flagToConfigKey {
		f := rootCmd.PersistentFlags().Lookup(name)
		if f == nil {
			f = searchCmd.PersistentFlags().Lookup(name)
		}
		if f == nil {
			panic(fmt.Sprintf("no such flag: %q", name))
		}
		if err := viper.BindPFlag(key, f); err != nil {
			panic(fmt.Sprintf("failed to bind flag %q: %v", name, err))
		}
	}
}

// unsignedSetting reads a count that cannot be negative. On the command line
// the flags are unsigned, so pflag refuses a negative value by itself. A value
// from the config file or the environment gets no such check: viper.GetUint
// drops the conversion error and returns 0, which would hide the mistake, and
// GetInt would let a negative context through to the backends and wrap a
// negative limit round to a huge number. The result stays an int because the
// backends subtract with it, and unsigned arithmetic would wrap there instead.
func unsignedSetting(flag string) (int, error) {
	key := flagToConfigKey[flag]
	raw := viper.Get(key)
	n, err := cast.ToUintE(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid value %v for --%s (%s): %w", raw, flag, key, err)
	}
	if n > math.MaxInt32 {
		return 0, fmt.Errorf("invalid value %v for --%s (%s): too large", raw, flag, key)
	}
	return int(n), nil
}

// resolveSettings reads the effective value of every bound flag, so that
// settings coming from the config file are honoured when the flag was not
// passed on the command line. The -b/--backends value is not bound to the
// config and is left for the search command to fill in.
func resolveSettings() (settings, error) {
	var s settings
	s.debug = viper.GetBool("debug")
	s.stats = viper.GetBool("stats")
	limit, err := unsignedSetting("limit")
	if err != nil {
		return settings{}, err
	}
	s.limit = uint(limit)
	s.sort = viper.GetString("defaults.sort")
	if s.linesBefore, err = unsignedSetting("before"); err != nil {
		return settings{}, err
	}
	if s.linesAfter, err = unsignedSetting("after"); err != nil {
		return settings{}, err
	}
	searchContext, err := unsignedSetting("context")
	if err != nil {
		return settings{}, err
	}
	// -C supplies both sides; a side that was asked for explicitly wins
	if searchContext > 0 {
		if s.linesBefore == 0 {
			s.linesBefore = searchContext
		}
		if s.linesAfter == 0 {
			s.linesAfter = searchContext
		}
	}
	s.caseInsensitive = viper.GetBool("defaults.case_insensitive")
	s.searchInFilenames = viper.GetBool("defaults.search_in_filenames")
	s.matchFilename = viper.GetString("defaults.match_filename")
	s.timeout = viper.GetDuration("defaults.timeout")
	s.json = viper.GetBool("defaults.json")

	// only ever force colours off: the library has already worked out whether
	// stdout is a terminal, and that answer must not be overridden
	if s.noColor = viper.GetBool("no_color"); s.noColor {
		color.NoColor = true
	}

	if s.debug {
		logrus.SetLevel(logrus.DebugLevel)
	}
	return s, nil
}

// configPath is where the configuration is expected, for error messages.
func configPath(configFile string) string {
	if configFile != "" {
		return configFile
	}
	return filepath.Join(configdir.LocalConfig(progname), "config.yml")
}

// requireConfig loads the configuration for the commands that need one and
// resolves the settings. It is deliberately not attached to every command:
// config-example exists to produce the file in the first place, so demanding
// one before it can run is circular.
func requireConfig(cmd *cobra.Command) (settings, error) {
	// the flags parsed, so anything failing from here is not a usage problem
	// and printing the full usage text would only bury the real message
	cmd.SilenceUsage = true
	flags := cmd.Flags()
	debug, err := flags.GetBool("debug")
	if err != nil {
		return settings{}, err
	}
	// before the config is read, so that reading it can be debugged too
	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	}
	configFile, err := flags.GetString("config")
	if err != nil {
		return settings{}, err
	}
	return loadConfig(configFile)
}

// loadConfig reads the config file, or the default one if configFile is
// empty, into the global config and resolves the settings.
func loadConfig(configFile string) (settings, error) {
	if configFile != "" {
		viper.SetConfigFile(configFile)
	} else {
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(configdir.LocalConfig(progname))
	}
	// only CS_ variables count: without a prefix an unrelated DEBUG or STATS in
	// the shell would switch options on. The replacer makes the nested keys
	// reachable, e.g. CS_DEFAULTS_LIMIT for defaults.limit. NO_COLOR does not go
	// through here; fatih/color reads it by itself.
	viper.SetEnvPrefix("CS")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		// SetConfigFile reports a missing file as a path error rather than as
		// ConfigFileNotFoundError, which only covers the search-path case.
		if _, ok := err.(viper.ConfigFileNotFoundError); ok || errors.Is(err, fs.ErrNotExist) {
			return settings{}, fmt.Errorf("no configuration file at %s\n\nCreate one with:\n    %s config-example > %s",
				configPath(configFile), progname, configPath(configFile))
		}
		return settings{}, fmt.Errorf("failed to read config file %s: %w", configPath(configFile), err)
	}
	logrus.Debugf("Using config file %q", viper.ConfigFileUsed())

	config := getConfig()
	if err := viper.Unmarshal(config); err != nil {
		return settings{}, fmt.Errorf("failed to parse config file %s: %w", viper.ConfigFileUsed(), err)
	}
	if err := config.Validate(); err != nil {
		return settings{}, fmt.Errorf("invalid config file %s: %w", viper.ConfigFileUsed(), err)
	}

	return resolveSettings()
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List the available backends",
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		_, err := requireConfig(cmd)
		return err
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		config := getConfig()
		backendNames := make([]string, 0, len(config.Backends))
		for name := range config.Backends {
			backendNames = append(backendNames, name)
		}
		sort.Strings(backendNames)
		w := cmd.OutOrStdout()
		for idx, name := range backendNames {
			if _, err := fmt.Fprintf(w, "%d) %s (type=%q)\n", idx+1, name, config.Backends[name].Type); err != nil {
				return err
			}
		}
		return nil
	},
}

var configExampleCmd = &cobra.Command{
	Use:   "config-example",
	Short: "Print an example config file",
	Run: func(cmd *cobra.Command, args []string) {
		// explicitly stdout: cobra's own Print helpers target stderr, which
		// would break "cs config-example > config.yml"
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), configFileExample)
	},
}

type sorter func(codesearch.Results) codesearch.Results

func getSorter(method string) sorter {
	switch method {
	case "a-z":
		return func(r codesearch.Results) codesearch.Results {
			sort.SliceStable(r, func(i, j int) bool { return r[i].Path < r[j].Path })
			return r
		}
	case "z-a":
		return func(r codesearch.Results) codesearch.Results {
			sort.SliceStable(r, func(i, j int) bool { return r[i].Path > r[j].Path })
			return r
		}
	case "":
		return func(r codesearch.Results) codesearch.Results {
			return r
		}
	default:
		return nil
	}
}

func repoNameFromRes(res *codesearch.Result) string {
	var repoName string
	if res.Owner != "" {
		repoName = res.Owner
		if res.RepoName != "" {
			repoName += "/"
		}
	}
	repoName += res.RepoName
	return repoName
}

// splitHighlight cuts the line into the text before, inside and after the
// match. The offsets come from the backend, so they are clamped rather than
// trusted: an out-of-range highlight would otherwise panic in the printer,
// after the search has already succeeded.
func splitHighlight(line string, h [2]int) (head, match, tail string) {
	start, end := h[0], h[1]
	if start < 0 {
		start = 0
	}
	if start > len(line) {
		start = len(line)
	}
	if end < start {
		end = start
	}
	if end > len(line) {
		end = len(line)
	}
	return line[:start], line[start:end], line[end:]
}

// contextBlock renders the lines around a match, numbered relative to it.
func contextBlock(res *codesearch.Result) (before, after string) {
	for i, line := range res.Context.Before {
		before += linePrefix(res, res.Lineno-(len(res.Context.Before)-i)) + line + "\n"
	}
	for i, line := range res.Context.After {
		after += linePrefix(res, res.Lineno+i+1) + line + "\n"
	}
	if after != "" {
		after = "\n" + after
	}
	return before, after
}

// linePrefix is the "N: " printed before line n of a result. A Lineno of 0
// means the backend could not place the match in the file (GitHub falls back
// to the search fragment when the file changed or is too large), so the lines
// are printed bare: numbering them from 0 would show a 0 for the match and
// negative numbers above it.
func linePrefix(res *codesearch.Result, n int) string {
	if res.Lineno == 0 {
		return ""
	}
	return fmt.Sprintf("%d: ", n)
}

// printResult renders one result as formatted text. A file name search has no
// line to show, so it prints the location alone.
func printResult(w io.Writer, res *codesearch.Result, filenamesOnly bool) {
	location := fmt.Sprintf(
		"%s:%s:%s (%s)",
		res.Backend,
		textBold.Sprint(toAnsiURL(res.RepoURL, repoNameFromRes(res))),
		textBold.Sprint(toAnsiURL(res.FileURL, res.Path)),
		textBold.Sprint(res.Branch),
	)
	if filenamesOnly {
		_, _ = fmt.Fprintf(w, "%s\n\n", location)
		return
	}
	before, after := contextBlock(res)
	head, match, tail := splitHighlight(res.Line, res.Highlight)
	var lineno string
	if res.Lineno != 0 {
		lineno = textBoldGreen.Sprint(res.Lineno) + ": "
	}
	_, _ = fmt.Fprintf(
		w,
		"%s\n\n%s%s%s%s\n\n",
		location,
		before,
		lineno,
		head+textBoldRed.Sprint(match)+tail,
		after,
	)
}

// jsonOutput is the machine-readable form of a run. Backends are reported
// alongside the results so a caller can tell a genuinely empty search from one
// where a backend failed.
type jsonOutput struct {
	Results      []*codesearch.Result `json:"results"`
	Backends     []jsonBackend        `json:"backends"`
	TotalResults int                  `json:"total_results"`
	DurationMS   int64                `json:"duration_ms"`
}

type jsonBackend struct {
	Name       string                 `json:"name"`
	Type       codesearch.BackendType `json:"type"`
	Results    int                    `json:"results"`
	DurationMS int64                  `json:"duration_ms"`
	Error      string                 `json:"error,omitempty"`
}

func writeJSON(w io.Writer, outcomes []searchOutcome, totalResults int, total time.Duration) error {
	out := jsonOutput{
		Results:      []*codesearch.Result{},
		Backends:     make([]jsonBackend, 0, len(outcomes)),
		TotalResults: totalResults,
		DurationMS:   total.Milliseconds(),
	}
	for _, o := range outcomes {
		out.Results = append(out.Results, o.selected...)
		b := jsonBackend{
			Name:       o.backend.Name(),
			Type:       o.backend.Type(),
			Results:    len(o.selected),
			DurationMS: o.duration.Milliseconds(),
		}
		if o.err != nil {
			b.Error = o.err.Error()
		}
		out.Backends = append(out.Backends, b)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// backendLimit is the result limit that can safely be pushed down into the
// backends. Nothing downstream may reorder or discard results for this to hold:
// --sort needs the whole set before it means anything, --match-filename can
// reject anything a backend returns, and -F deduplicates and re-sorts by path.
// In those cases the backend fetches everything and the truncation stays where
// it was, at print time.
func backendLimit(s settings) uint {
	if s.sort != "" || s.matchFilename != "" || s.searchInFilenames {
		return 0
	}
	return s.limit
}

// searchOutcome is what one backend produced, including a failure. Failures
// are carried rather than raised so that one unreachable backend does not
// discard the results of the others.
type searchOutcome struct {
	backend  codesearch.Backend
	results  codesearch.Results
	selected []*codesearch.Result
	// shown is how many results were printed
	shown    int
	duration time.Duration
	err      error
}

// runSearches queries every backend concurrently, so the wall clock cost of a
// search is the slowest backend rather than the sum of all of them. Outcomes
// are returned in the order the backends were given, not the order they
// finished, so the output stays stable across runs.
func runSearches(ctx context.Context, backends []codesearch.Backend, searchString string, opts codesearch.SearchOptions) []searchOutcome {
	return runSearchesLive(ctx, backends, searchString, opts, nil)
}

// runSearchesLive is runSearches that also prints the results through live
// while the searches run, if live is not nil.
func runSearchesLive(ctx context.Context, backends []codesearch.Backend, searchString string, opts codesearch.SearchOptions, live *livePrinter) []searchOutcome {
	type indexed struct {
		idx     int
		outcome searchOutcome
	}
	ch := make(chan indexed, len(backends))
	for idx, b := range backends {
		go func(idx int, b codesearch.Backend) {
			start := time.Now()
			opts := opts
			if live != nil {
				opts.OnResult = live.streams[idx].add
			}
			results, err := b.Search(ctx, searchString, opts)
			if live != nil {
				live.finish(idx, results)
			}
			if err != nil {
				// reported once, in the summary at the end
				logrus.Debugf("Backend %q failed: %v", b.Name(), err)
			}
			ch <- indexed{idx, searchOutcome{
				backend:  b,
				results:  results,
				duration: time.Since(start),
				err:      err,
			}}
		}(idx, b)
	}
	outcomes := make([]searchOutcome, len(backends))
	for range backends {
		r := <-ch
		outcomes[r.idx] = r.outcome
	}
	if live != nil {
		// the summary on stderr comes after the last result
		live.close()
	}
	return outcomes
}

// liveStream prints the results of one backend. It applies the same selection
// as selectResults, one result at a time.
type liveStream struct {
	mu            sync.Mutex
	out           *liveWriter
	limit         uint
	matchFilename *regexp.Regexp
	// live says the results go straight to w; otherwise they wait in pending
	// for the backends before this one to finish
	live    bool
	pending []codesearch.Result
	// received counts the results the backend passed on while it searched
	received int
	shown    int
}

// add takes the next result of the backend. It is the backend's OnResult.
func (s *liveStream) add(res codesearch.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received++
	s.take(res)
}

func (s *liveStream) take(res codesearch.Result) {
	if res.IsFilename {
		return
	}
	if s.matchFilename != nil && !s.matchFilename.MatchString(res.Path) {
		return
	}
	if s.limit > 0 && uint(s.shown) >= s.limit {
		return
	}
	s.shown++
	if s.live {
		s.out.put(res)
		return
	}
	s.pending = append(s.pending, res)
}

// finish takes the results that Search returned. A backend may pass on all,
// some or none of its results while it searches; what it did not pass on is
// the rest of what it returns.
func (s *liveStream) finish(results codesearch.Results) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := s.received; i < len(results); i++ {
		s.take(results[i])
	}
}

// goLive prints what has been waiting and lets later results through.
func (s *liveStream) goLive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, res := range s.pending {
		s.out.put(res)
	}
	s.pending = nil
	s.live = true
}

// liveWriter prints results from a goroutine of its own. The backends hand
// their results over without waiting for the output: stdout can be slow, or
// stand still for as long as a pager is not scrolled, and a search that waited
// for it would run into --timeout through no fault of the backend.
type liveWriter struct {
	w      io.Writer
	mu     sync.Mutex
	wake   *sync.Cond
	queue  []codesearch.Result
	closed bool
	done   chan struct{}
}

func newLiveWriter(w io.Writer) *liveWriter {
	q := &liveWriter{w: w, done: make(chan struct{})}
	q.wake = sync.NewCond(&q.mu)
	go q.run()
	return q
}

// put queues a result for printing. It does not block on the output.
func (q *liveWriter) put(res codesearch.Result) {
	q.mu.Lock()
	q.queue = append(q.queue, res)
	q.mu.Unlock()
	q.wake.Signal()
}

// close prints what is still queued and stops the goroutine.
func (q *liveWriter) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.wake.Signal()
	<-q.done
}

func (q *liveWriter) run() {
	defer close(q.done)
	for {
		q.mu.Lock()
		for len(q.queue) == 0 && !q.closed {
			q.wake.Wait()
		}
		batch := q.queue
		q.queue = nil
		q.mu.Unlock()
		if len(batch) == 0 {
			// closed, and nothing left
			return
		}
		for i := range batch {
			printResult(q.w, &batch[i], false)
		}
	}
}

// livePrinter prints results while the searches are still running, instead
// of after the slowest backend is done. The output is the same as printing
// everything at the end: the results of the first backend are printed as they
// arrive, and those of each later backend are held back until every backend
// before it has finished.
type livePrinter struct {
	out     *liveWriter
	streams []*liveStream
	mu      sync.Mutex
	done    []bool
	// head is the backend whose results are being printed
	head int
}

func newLivePrinter(w io.Writer, backends int, limit uint, matchFilename *regexp.Regexp) *livePrinter {
	p := &livePrinter{out: newLiveWriter(w), done: make([]bool, backends)}
	for range backends {
		p.streams = append(p.streams, &liveStream{out: p.out, limit: limit, matchFilename: matchFilename})
	}
	if backends > 0 {
		p.streams[0].goLive()
	}
	return p
}

// finish records that a backend is done, with the results it returned, and
// moves on to the next backend that is not.
func (p *livePrinter) finish(idx int, results codesearch.Results) {
	p.streams[idx].finish(results)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done[idx] = true
	for p.head < len(p.streams) && p.done[p.head] {
		p.head++
		if p.head < len(p.streams) {
			p.streams[p.head].goLive()
		}
	}
}

// close waits until every result is on the output. No result may be added
// after it.
func (p *livePrinter) close() {
	p.out.close()
}

// shown returns how many results of a backend were printed.
func (p *livePrinter) shown(idx int) int {
	s := p.streams[idx]
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shown
}

// canPrintLive reports whether results can be printed before the search is
// over. JSON is one document, --sort needs every result before it knows the
// first one, and -F deduplicates and sorts.
func canPrintLive(s settings) bool {
	return !s.json && s.sort == "" && !s.searchInFilenames
}

func failedSearches(outcomes []searchOutcome) []searchOutcome {
	var failed []searchOutcome
	for _, o := range outcomes {
		if o.err != nil {
			failed = append(failed, o)
		}
	}
	return failed
}

// capabilityFlags names the command line flag that requests each capability,
// so an unsupported combination can be reported in the user's terms.
var capabilityFlags = map[codesearch.Capability]string{
	codesearch.CapCaseInsensitive:   "--case-insensitive/-i",
	codesearch.CapSearchInFilenames: "--search-in-filenames/-F",
}

func requestedCapabilities(s settings) []codesearch.Capability {
	var caps []codesearch.Capability
	if s.caseInsensitive {
		caps = append(caps, codesearch.CapCaseInsensitive)
	}
	if s.searchInFilenames {
		caps = append(caps, codesearch.CapSearchInFilenames)
	}
	return caps
}

// checkCapabilities reports every backend that cannot honour a requested
// option. Silently ignoring these produces results that look complete but
// quietly answer a different question.
func checkCapabilities(backends []codesearch.Backend, caps []codesearch.Capability) error {
	var problems []string
	for _, b := range backends {
		for _, c := range caps {
			if !b.Supports(c) {
				problems = append(problems, fmt.Sprintf(
					"  backend %q (type %q) does not support %s, requested by %s",
					b.Name(), b.Type(), c, capabilityFlags[c]))
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "\n"))
}

// compileFilenamePattern turns --match-filename into a matcher. The pattern is
// an unanchored, case-insensitive regexp, so a plain substring like ".go"
// still behaves the way the previous strings.Contains did while "\\.go$" or
// "^cmd/" now mean what they look like. Case sensitivity can be asked for with
// an inline (?-i).
func compileFilenamePattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid --match-filename pattern %q: %w", pattern, err)
	}
	return re, nil
}

// selectResults picks the content matches to print, applying the file name
// filter first so that the limit counts results that are actually shown
// rather than results merely examined.
func selectResults(results codesearch.Results, limit uint, matchFilename *regexp.Regexp) []*codesearch.Result {
	var out []*codesearch.Result
	for i := range results {
		res := &results[i]
		if res.IsFilename {
			continue
		}
		if matchFilename != nil && !matchFilename.MatchString(res.Path) {
			continue
		}
		if limit > 0 && uint(len(out)) >= limit {
			break
		}
		out = append(out, res)
	}
	return out
}

// selectFilenames picks the unique file names to print for a -F search. The
// backend has already matched the pattern against the file names, so nothing
// is re-tested here: a substring check would throw away every hit from a
// pattern that is not a literal, which is exactly what csearch searches with.
// A file is identified by its repository as well as its path, because a
// csearch index with several roots holds paths that are only relative to
// their root. sorted says the results are already in the order requested by
// --sort; otherwise they are put in path order. The limit counts distinct file
// names in the final order, since that is what gets printed.
func selectFilenames(results codesearch.Results, limit uint, sorted bool) []*codesearch.Result {
	type file struct{ repoURL, path string }
	seen := make(map[file]struct{}, len(results))
	var out []*codesearch.Result
	for i := range results {
		res := &results[i]
		key := file{res.RepoURL, res.Path}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, res)
	}
	if !sorted {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Path != out[j].Path {
				return out[i].Path < out[j].Path
			}
			return out[i].RepoURL < out[j].RepoURL
		})
	}
	if limit > 0 && uint(len(out)) > limit {
		out = out[:limit]
	}
	return out
}

// resolveBackendNames turns the requested backend names into keys of the
// config. "all" selects every backend, sorted so that the output order is the
// same on every run rather than following map iteration. Names are matched
// without regard to case, and blanks and repeats are dropped so that
// -b "a, b" works and -b a,a does not search twice.
func resolveBackendNames(config *codesearch.Config, requested []string) ([]string, error) {
	var names []string
	seen := make(map[string]struct{}, len(requested))
	for _, name := range requested {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.EqualFold(name, codesearch.AllBackends) {
			names = make([]string, 0, len(config.Backends))
			for key := range config.Backends {
				names = append(names, key)
			}
			sort.Strings(names)
			return names, nil
		}
		key, ok := config.LookupBackend(name)
		if !ok {
			return nil, fmt.Errorf("backend %q not found", name)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		names = append(names, key)
	}
	return names, nil
}

var searchCmd = &cobra.Command{
	Use:   "search",
	Short: "Search code in the specified backends",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// loaded here rather than in PersistentPreRunE, which has no way to
		// hand the settings on to RunE
		s, err := requireConfig(cmd)
		if err != nil {
			return err
		}
		if s.backends, err = cmd.Flags().GetString("backends"); err != nil {
			return err
		}

		// a cancelled run must abandon in-flight requests rather than wait
		// for them, so Ctrl-C is honoured mid-search
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		err = runSearch(ctx, getConfig(), s, args, cmd.OutOrStdout(), cmd.ErrOrStderr())
		if errors.Is(err, errReported) {
			// the summary is on stderr already; cobra's "Error:" line
			// would only repeat it
			cmd.SilenceErrors = true
		}
		return err
	},
}

// errReported marks a failure that runSearch has already described on stderr
// in a form cobra's one-line "Error:" cannot match. It still has to reach
// main, which turns it into the exit status.
var errReported = errors.New("failure already reported")

// runSearch runs one search across the selected backends. It prints the
// results to stdout and the progress, stats and failures to stderr. Results
// from the backends that worked are printed even when others fail, and the
// failure is then returned so that the exit status reflects it.
func runSearch(ctx context.Context, config *codesearch.Config, s settings, args []string, stdout, stderr io.Writer) error {
	// get backends. Command line overrides config file, and "all" expands
	// to all known backends in the config file.
	var backendsToValidate = config.DefaultBackends
	if s.backends != "" {
		backendsToValidate = strings.Split(s.backends, ",")
	}
	backendNames, err := resolveBackendNames(config, backendsToValidate)
	if err != nil {
		return err
	}
	// get sorter to sort results later
	sorter := getSorter(s.sort)
	if sorter == nil {
		// the wording predates this function and users may match on it
		return errors.New("Invalid value for --sort") //nolint:staticcheck // ST1005

	}
	matchFilename, err := compileFilenamePattern(s.matchFilename)
	if err != nil {
		return err
	}

	searchString := strings.Join(args, " ")
	_, _ = fmt.Fprintf(stderr, "Searching %q on %q\n", searchString, backendNames)
	backends := make([]codesearch.Backend, 0, len(backendNames))
	for _, name := range backendNames {
		backend, err := codesearch.NewBackend(name, config.Backends[name])
		if err != nil {
			return err
		}
		backends = append(backends, backend)
	}
	if len(backends) == 0 {
		return errors.New("no backends specified")
	}
	if err := checkCapabilities(backends, requestedCapabilities(s)); err != nil {
		// printed rather than returned so the per-backend list stays readable
		_, _ = fmt.Fprintf(stderr, "Requested options are not supported:\n%v\n", err)
		return errReported
	}

	searchStart := time.Now()
	var live *livePrinter
	if canPrintLive(s) {
		live = newLivePrinter(stdout, len(backends), s.limit, matchFilename)
	}
	outcomes := runSearchesLive(ctx, backends, searchString, codesearch.SearchOptions{
		LinesBefore:       s.linesBefore,
		LinesAfter:        s.linesAfter,
		CaseInsensitive:   s.caseInsensitive,
		SearchInFilenames: s.searchInFilenames,
		Timeout:           s.timeout,
		Limit:             backendLimit(s),
	}, live)
	totalResults := 0
	for i := range outcomes {
		o := &outcomes[i]
		if live != nil {
			// already printed, and that includes what a backend found
			// before it failed
			o.shown = live.shown(i)
			totalResults += o.shown
			continue
		}
		if o.err != nil {
			continue
		}
		// sort the results, if requested
		results := sorter(o.results)
		if s.searchInFilenames {
			o.selected = selectFilenames(results, s.limit, s.sort != "")
		} else {
			o.selected = selectResults(results, s.limit, matchFilename)
		}
		o.shown = len(o.selected)
		totalResults += o.shown
	}
	totalTime := time.Since(searchStart)

	if s.json {
		if err := writeJSON(stdout, outcomes, totalResults, totalTime); err != nil {
			return fmt.Errorf("failed to write JSON output: %w", err)
		}
	} else if live == nil {
		for _, o := range outcomes {
			for _, res := range o.selected {
				printResult(stdout, res, s.searchInFilenames)
			}
		}
	}
	if s.stats {
		for _, o := range outcomes {
			if o.err != nil {
				_, _ = fmt.Fprintf(stderr, "Failed on %q after %s\n", o.backend.Name(), o.duration)
				continue
			}
			_, _ = fmt.Fprintf(stderr, "Got %d results on %q in %s\n", o.shown, o.backend.Name(), o.duration)
		}
	}
	_, _ = fmt.Fprintf(stderr, "Got %d total results in %s\n", totalResults, totalTime)

	// Results from the backends that did work are already printed, so a
	// failure elsewhere is reported at the end and reflected in the exit
	// status rather than discarding everything.
	if failed := failedSearches(outcomes); len(failed) > 0 {
		_, _ = fmt.Fprintf(stderr, "\n%d of %d backends failed:\n", len(failed), len(outcomes))
		for _, o := range failed {
			_, _ = fmt.Fprintf(stderr, "  %s: %v\n", o.backend.Name(), o.err)
		}
		return fmt.Errorf("%d of %d backends failed: %w", len(failed), len(outcomes), errReported)
	}
	return nil
}

// toAnsiURL wraps text in an OSC 8 terminal hyperlink. The escapes are
// invisible in a terminal that understands them, but land verbatim in the
// output when it is redirected to a file or a pipe, so they follow the same
// switch as the colours: NO_COLOR, --no-color, or stdout not being a terminal.
func toAnsiURL(url, text string) string {
	if color.NoColor {
		return text
	}
	return fmt.Sprintf("\033]8;;%s\033\\%s\033]8;;\033\\", url, text)
}

var rootCmd = &cobra.Command{
	Use:   progname,
	Short: fmt.Sprintf("%q is a code searching tool inspired by Facebook's BigGrep.", progname),
	Long:  fmt.Sprintf("%s is a code searching tool inspired by Facebook's BigGrep. It can search across different code repository types (e.g. GitHub), and on the local filesystem using google/codesearch's `cindex` tool.", progname),
	Args:  cobra.MinimumNArgs(1),
	Run:   nil,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		// cobra, or runSearch for errReported, has already printed the message
		os.Exit(1)
	}
}
