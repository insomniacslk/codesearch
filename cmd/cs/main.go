package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/insomniacslk/codesearch/pkg/codesearch"
	"github.com/kirsle/configdir"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const progname = "cs"

//go:embed config.yml.example
var configFileExample string

var (
	globalConfig codesearch.Config

	configFile              string
	flagDebug               bool
	flagStats               bool
	flagSearchInFilenames   bool
	flagMatchFilename       string
	flagSearchContextBefore int
	flagSearchContextAfter  int
	flagSearchContext       int
	flagCaseInsensitive     bool
	flagLimit               uint
	flagSort                string
	flagTimeout             time.Duration
	flagJSON                bool
	flagNoColor             bool

	searchBackends string

	textBold      = color.New(color.Bold)
	textBoldGreen = color.New(color.FgGreen, color.Bold)
	textBoldRed   = color.New(color.FgRed, color.Bold)
)

func getConfig() *codesearch.Config {
	return &globalConfig
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "", "Configuration file")
	rootCmd.PersistentFlags().BoolVarP(&flagDebug, "debug", "d", false, "Print debug messages")
	rootCmd.PersistentFlags().BoolVarP(&flagStats, "stats", "S", false, "Print stats")
	rootCmd.PersistentFlags().BoolVar(&flagNoColor, "no-color", false, "Disable colours and terminal hyperlinks. They are disabled automatically when stdout is not a terminal, or when NO_COLOR is set")

	searchCmd.PersistentFlags().StringVarP(&searchBackends, "backends", "b", "", "Comma-separated list of names of the backends to use. The names are defined in your configuration file. If specified, it overrides `default_backends` in the configuration file. \"all\" will use every backend")
	searchCmd.PersistentFlags().BoolVarP(&flagSearchInFilenames, "search-in-filenames", "F", false, "Search only in file names")
	searchCmd.PersistentFlags().StringVarP(&flagMatchFilename, "match-filename", "f", "", "Show results only from files whose path matches this regexp. Unanchored and case-insensitive, so \".go\" matches anywhere in the path and \"\\.go$\" only at the end")
	searchCmd.PersistentFlags().IntVarP(&flagSearchContextBefore, "before", "B", 0, "Number of context lines to show before the result")
	searchCmd.PersistentFlags().IntVarP(&flagSearchContextAfter, "after", "A", 0, "Number of context lines to show after the result")
	searchCmd.PersistentFlags().IntVarP(&flagSearchContext, "context", "C", 0, "Number of context lines to show on both sides of the result. A non-zero --before or --after takes precedence over it")
	searchCmd.PersistentFlags().BoolVarP(&flagCaseInsensitive, "case-insensitive", "i", false, "Case-insensitive search")
	searchCmd.PersistentFlags().UintVarP(&flagLimit, "limit", "l", 0, "Limit the amount of results that are printed per backend. 0 means no limit. Unless --sort, --match-filename or -F is in play, backends stop fetching once the limit is met, which matters on rate-limited APIs")
	searchCmd.PersistentFlags().StringVarP(&flagSort, "sort", "s", "", "Sort the results. Possible values: \"a-z\", \"z-a\"")
	searchCmd.PersistentFlags().DurationVarP(&flagTimeout, "timeout", "t", 0, "Maximum time to spend on each backend, e.g. \"30s\" or \"2m\". 0 means no limit")
	searchCmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "Print results as JSON on stdout instead of formatted text")

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

// resolveFlags applies the effective value of every bound flag, so that
// settings coming from the config file are honoured when the flag was not
// passed on the command line.
func resolveFlags() {
	flagDebug = viper.GetBool("debug")
	flagStats = viper.GetBool("stats")
	flagLimit = uint(viper.GetInt("defaults.limit"))
	flagSort = viper.GetString("defaults.sort")
	flagSearchContextBefore = viper.GetInt("defaults.before")
	flagSearchContextAfter = viper.GetInt("defaults.after")
	// -C supplies both sides; a side that was asked for explicitly wins
	if flagSearchContext = viper.GetInt("defaults.context"); flagSearchContext > 0 {
		if flagSearchContextBefore == 0 {
			flagSearchContextBefore = flagSearchContext
		}
		if flagSearchContextAfter == 0 {
			flagSearchContextAfter = flagSearchContext
		}
	}
	flagCaseInsensitive = viper.GetBool("defaults.case_insensitive")
	flagSearchInFilenames = viper.GetBool("defaults.search_in_filenames")
	flagMatchFilename = viper.GetString("defaults.match_filename")
	flagTimeout = viper.GetDuration("defaults.timeout")
	flagJSON = viper.GetBool("defaults.json")

	// only ever force colours off: the library has already worked out whether
	// stdout is a terminal, and that answer must not be overridden
	if flagNoColor = viper.GetBool("no_color"); flagNoColor {
		color.NoColor = true
	}

	if flagDebug {
		logrus.SetLevel(logrus.DebugLevel)
	}
}

// configPath is where the configuration is expected, for error messages.
func configPath() string {
	if configFile != "" {
		return configFile
	}
	return filepath.Join(configdir.LocalConfig(progname), "config.yml")
}

// requireConfig loads the configuration for the commands that need one. It is
// deliberately not attached to every command: config-example exists to produce
// the file in the first place, so demanding one before it can run is circular.
func requireConfig(cmd *cobra.Command, _ []string) error {
	// the flags parsed, so anything failing from here is not a usage problem
	// and printing the full usage text would only bury the real message
	cmd.SilenceUsage = true
	return loadConfig()
}

func loadConfig() error {
	if flagDebug {
		logrus.SetLevel(logrus.DebugLevel)
	}
	if configFile != "" {
		viper.SetConfigFile(configFile)
	} else {
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(configdir.LocalConfig(progname))
	}
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		// SetConfigFile reports a missing file as a path error rather than as
		// ConfigFileNotFoundError, which only covers the search-path case.
		if _, ok := err.(viper.ConfigFileNotFoundError); ok || errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no configuration file at %s\n\nCreate one with:\n    %s config-example > %s",
				configPath(), progname, configPath())
		}
		return fmt.Errorf("failed to read config file %s: %w", configPath(), err)
	}
	logrus.Debugf("Using config file %q", viper.ConfigFileUsed())

	config := getConfig()
	if err := viper.Unmarshal(config); err != nil {
		return fmt.Errorf("failed to parse config file %s: %w", viper.ConfigFileUsed(), err)
	}
	if err := config.Validate(); err != nil {
		return fmt.Errorf("invalid config file %s: %w", viper.ConfigFileUsed(), err)
	}

	resolveFlags()
	return nil
}

var listCmd = &cobra.Command{
	Use:               "list",
	Short:             "List the available backends",
	PersistentPreRunE: requireConfig,
	Run: func(cmd *cobra.Command, args []string) {
		config := getConfig()
		backendNames := make([]string, 0, len(config.Backends))
		for name := range config.Backends {
			backendNames = append(backendNames, name)
		}
		sort.Strings(backendNames)
		for idx, name := range backendNames {
			fmt.Printf("%d) %s (type=%q)\n", idx+1, name, config.Backends[name].Type)
		}
	},
}

var configExampleCmd = &cobra.Command{
	Use:   "config-example",
	Short: "Print an example config file",
	Run: func(cmd *cobra.Command, args []string) {
		// explicitly stdout: cobra's own Print helpers target stderr, which
		// would break "cs config-example > config.yml"
		fmt.Fprintln(cmd.OutOrStdout(), configFileExample)
	},
}

type sorter func(codesearch.Results) codesearch.Results

func getSorter(method string) sorter {
	switch method {
	case "a-z":
		return func(r codesearch.Results) codesearch.Results {
			sort.Slice(r, func(i, j int) bool { return r[i].Path < r[j].Path })
			return r
		}
	case "z-a":
		return func(r codesearch.Results) codesearch.Results {
			sort.Slice(r, func(i, j int) bool { return r[i].Path > r[j].Path })
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
		fmt.Fprintf(w, "%s\n\n", location)
		return
	}
	before, after := contextBlock(res)
	head, match, tail := splitHighlight(res.Line, res.Highlight)
	var lineno string
	if res.Lineno != 0 {
		lineno = textBoldGreen.Sprint(res.Lineno) + ": "
	}
	fmt.Fprintf(
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
	Name       string `json:"name"`
	Type       string `json:"type"`
	Results    int    `json:"results"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
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
func backendLimit() uint {
	if flagSort != "" || flagMatchFilename != "" || flagSearchInFilenames {
		return 0
	}
	return flagLimit
}

// searchOutcome is what one backend produced, including a failure. Failures
// are carried rather than raised so that one unreachable backend does not
// discard the results of the others.
type searchOutcome struct {
	backend  codesearch.Backend
	results  codesearch.Results
	selected []*codesearch.Result
	duration time.Duration
	err      error
}

// runSearches queries every backend concurrently, so the wall clock cost of a
// search is the slowest backend rather than the sum of all of them. Outcomes
// are returned in the order the backends were given, not the order they
// finished, so the output stays stable across runs.
func runSearches(ctx context.Context, backends []codesearch.Backend, searchString string, opts codesearch.SearchOptions) []searchOutcome {
	type indexed struct {
		idx     int
		outcome searchOutcome
	}
	ch := make(chan indexed, len(backends))
	for idx, b := range backends {
		go func(idx int, b codesearch.Backend) {
			start := time.Now()
			results, err := b.Search(ctx, searchString, opts)
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
	return outcomes
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

func requestedCapabilities() []codesearch.Capability {
	var caps []codesearch.Capability
	if flagCaseInsensitive {
		caps = append(caps, codesearch.CapCaseInsensitive)
	}
	if flagSearchInFilenames {
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
// The limit counts distinct file names, since that is what gets printed.
func selectFilenames(results codesearch.Results, limit uint) []*codesearch.Result {
	seen := make(map[string]struct{}, len(results))
	var out []*codesearch.Result
	for i := range results {
		res := &results[i]
		if _, ok := seen[res.Path]; ok {
			continue
		}
		if limit > 0 && uint(len(out)) >= limit {
			break
		}
		seen[res.Path] = struct{}{}
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

var searchCmd = &cobra.Command{
	Use:               "search",
	Short:             "Search code in the specified backends",
	Args:              cobra.MinimumNArgs(1),
	PersistentPreRunE: requireConfig,
	Run: func(cmd *cobra.Command, args []string) {
		config := getConfig()

		// get backends. Command line overrides config file, and "all" expands
		// to all known backends in the config file.
		var backendsToValidate = config.DefaultBackends
		if searchBackends != "" {
			backendsToValidate = strings.Split(searchBackends, ",")
		}
		var backendNames []string
		for _, b := range backendsToValidate {
			// Config.Validate has already ensured that there is either "all" or
			// individual backend names.
			if b == "all" {
				// clear out backendNames in case other backends were
				// appended
				backendNames = make([]string, 0, len(config.Backends))
				for name := range config.Backends {
					backendNames = append(backendNames, name)
				}
				break
			} else {
				backendNames = append(backendNames, b)
			}
		}
		// get sorter to sort results later
		sorter := getSorter(flagSort)
		if sorter == nil {
			log.Fatalf("Invalid value for --sort")
		}
		matchFilename, err := compileFilenamePattern(flagMatchFilename)
		if err != nil {
			logrus.Fatal(err)
		}

		searchString := strings.Join(args, " ")
		fmt.Fprintf(os.Stderr, "Searching %q on %q\n", searchString, backendNames)
		backends := make([]codesearch.Backend, 0, len(backendNames))
		for _, name := range backendNames {
			backendConfig, ok := config.Backends[name]
			if !ok {
				logrus.Fatalf("Backend %q not found", name)
			}
			backend := codesearch.BackendByType(backendConfig.Type)
			if backend == nil {
				logrus.Fatalf("Failed to get backend for type %q", backendConfig.Type)
			}
			var err error
			backend, err = backend.New(
				name, backendConfig.Params,
			)
			if err != nil {
				logrus.Fatalf("Failed to instantiate backend %q: %v", name, err)
			}
			backends = append(backends, backend)
		}
		if len(backends) == 0 {
			logrus.Fatal("No backends specified")
		}
		if err := checkCapabilities(backends, requestedCapabilities()); err != nil {
			// printed rather than logged so the per-backend list stays readable
			fmt.Fprintf(os.Stderr, "Requested options are not supported:\n%v\n", err)
			os.Exit(1)
		}
		// a cancelled run must abandon in-flight requests rather than wait
		// for them, so Ctrl-C is honoured mid-search
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		searchStart := time.Now()
		outcomes := runSearches(ctx, backends, searchString, codesearch.SearchOptions{
			LinesBefore:       flagSearchContextBefore,
			LinesAfter:        flagSearchContextAfter,
			CaseInsensitive:   flagCaseInsensitive,
			SearchInFilenames: flagSearchInFilenames,
			Timeout:           flagTimeout,
			Limit:             backendLimit(),
		})
		totalResults := 0
		for i := range outcomes {
			o := &outcomes[i]
			if o.err != nil {
				continue
			}
			// sort the results, if requested
			results := sorter(o.results)
			if flagSearchInFilenames {
				o.selected = selectFilenames(results, flagLimit)
			} else {
				o.selected = selectResults(results, flagLimit, matchFilename)
			}
			totalResults += len(o.selected)
		}
		totalTime := time.Since(searchStart)

		if flagJSON {
			if err := writeJSON(os.Stdout, outcomes, totalResults, totalTime); err != nil {
				logrus.Fatalf("Failed to write JSON output: %v", err)
			}
		} else {
			for _, o := range outcomes {
				for _, res := range o.selected {
					printResult(os.Stdout, res, flagSearchInFilenames)
				}
			}
		}
		if flagStats {
			for _, o := range outcomes {
				if o.err != nil {
					fmt.Fprintf(os.Stderr, "Failed on %q after %s\n", o.backend.Name(), o.duration)
					continue
				}
				fmt.Fprintf(os.Stderr, "Got %d results on %q in %s\n", len(o.selected), o.backend.Name(), o.duration)
			}
		}
		fmt.Fprintf(os.Stderr, "Got %d total results in %s\n", totalResults, totalTime)

		// Results from the backends that did work are already printed, so a
		// failure elsewhere is reported at the end and reflected in the exit
		// status rather than discarding everything.
		if failed := failedSearches(outcomes); len(failed) > 0 {
			fmt.Fprintf(os.Stderr, "\n%d of %d backends failed:\n", len(failed), len(outcomes))
			for _, o := range failed {
				fmt.Fprintf(os.Stderr, "  %s: %v\n", o.backend.Name(), o.err)
			}
			os.Exit(1)
		}
	},
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
		// cobra has already printed the message
		os.Exit(1)
	}
}
