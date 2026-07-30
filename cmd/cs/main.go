package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"sort"
	"strings"
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
	flagCaseInsensitive     bool
	flagLimit               uint
	flagSort                string
	flagTimeout             time.Duration

	searchBackends string

	textBold      = color.New(color.Bold)
	textBoldGreen = color.New(color.FgGreen, color.Bold)
	textBoldRed   = color.New(color.FgRed, color.Bold)
)

func getConfig() *codesearch.Config {
	return &globalConfig
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "", "Configuration file")
	rootCmd.PersistentFlags().BoolVarP(&flagDebug, "debug", "d", false, "Print debug messages")
	rootCmd.PersistentFlags().BoolVarP(&flagStats, "stats", "S", false, "Print stats")

	searchCmd.PersistentFlags().StringVarP(&searchBackends, "backends", "b", "", "Comma-separated list of names of the backends to use. The names are defined in your configuration file. If specified, it overrides `default_backends` in the configuration file. \"all\" will use every backend")
	searchCmd.PersistentFlags().BoolVarP(&flagSearchInFilenames, "search-in-filenames", "F", false, "Search only in file names")
	searchCmd.PersistentFlags().StringVarP(&flagMatchFilename, "match-filename", "f", "", "Show results only from files whose names match the provided pattern")
	searchCmd.PersistentFlags().IntVarP(&flagSearchContextBefore, "before", "B", 0, "Number of context lines to show before the result")
	searchCmd.PersistentFlags().IntVarP(&flagSearchContextAfter, "after", "A", 0, "Number of context lines to show after the result")
	searchCmd.PersistentFlags().BoolVarP(&flagCaseInsensitive, "case-insensitive", "i", false, "Case-insensitive search")
	searchCmd.PersistentFlags().UintVarP(&flagLimit, "limit", "l", 0, "Limit the amount of results that are printed per backend. 0 means no limit")
	searchCmd.PersistentFlags().StringVarP(&flagSort, "sort", "s", "", "Sort the results. Possible values: \"a-z\", \"z-a\"")
	searchCmd.PersistentFlags().DurationVarP(&flagTimeout, "timeout", "t", 0, "Maximum time to spend on each backend, e.g. \"30s\" or \"2m\". 0 means no limit")

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
	"limit":               "defaults.limit",
	"sort":                "defaults.sort",
	"before":              "defaults.before",
	"after":               "defaults.after",
	"case-insensitive":    "defaults.case_insensitive",
	"search-in-filenames": "defaults.search_in_filenames",
	"match-filename":      "defaults.match_filename",
	"timeout":             "defaults.timeout",
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
	flagCaseInsensitive = viper.GetBool("defaults.case_insensitive")
	flagSearchInFilenames = viper.GetBool("defaults.search_in_filenames")
	flagMatchFilename = viper.GetString("defaults.match_filename")
	flagTimeout = viper.GetDuration("defaults.timeout")

	if flagDebug {
		logrus.SetLevel(logrus.DebugLevel)
	}
}

func initConfig() {
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
			// TODO create new config file
			logrus.Fatalf("Config file not found")
		}
		logrus.Fatalf("Failed to read config file: %v", err)
	}
	logrus.Debugf("Using config file %q", viper.ConfigFileUsed())

	config := getConfig()
	if err := viper.Unmarshal(config); err != nil {
		logrus.Fatalf("Failed to unmarshal config: %v", err)
	}
	if err := config.Validate(); err != nil {
		logrus.Fatalf("Invalid config: %v", err)
	}

	resolveFlags()
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List the available backends",
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
		fmt.Println(configFileExample)
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

// searchOutcome is what one backend produced, including a failure. Failures
// are carried rather than raised so that one unreachable backend does not
// discard the results of the others.
type searchOutcome struct {
	backend  codesearch.Backend
	results  codesearch.Results
	duration time.Duration
	printed  int
	err      error
}

// runSearches queries every backend concurrently, so the wall clock cost of a
// search is the slowest backend rather than the sum of all of them. Outcomes
// are returned in the order the backends were given, not the order they
// finished, so the output stays stable across runs.
func runSearches(backends []codesearch.Backend, searchString string, opts ...codesearch.Opt) []searchOutcome {
	type indexed struct {
		idx     int
		outcome searchOutcome
	}
	ch := make(chan indexed, len(backends))
	for idx, b := range backends {
		go func(idx int, b codesearch.Backend) {
			start := time.Now()
			results, err := b.Search(searchString, opts...)
			if err != nil {
				logrus.Errorf("Backend %q failed: %v", b.Name(), err)
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

// selectResults picks the content matches to print, applying the file name
// filter first so that the limit counts results that are actually shown
// rather than results merely examined.
func selectResults(results codesearch.Results, limit uint, matchFilename string) []*codesearch.Result {
	var out []*codesearch.Result
	for i := range results {
		res := &results[i]
		if res.IsFilename {
			continue
		}
		if matchFilename != "" && !strings.Contains(strings.ToLower(res.Path), strings.ToLower(matchFilename)) {
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
	Use:   "search",
	Short: "Search code in the specified backends",
	Args:  cobra.MinimumNArgs(1),
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
		searchStart := time.Now()
		outcomes := runSearches(backends, searchString,
			codesearch.WithLinesBefore(flagSearchContextBefore),
			codesearch.WithLinesAfter(flagSearchContextAfter),
			codesearch.WithCaseInsensitive(flagCaseInsensitive),
			codesearch.WithSearchInFilenames(flagSearchInFilenames),
			codesearch.WithTimeout(flagTimeout),
		)
		totalResults := 0
		for i := range outcomes {
			o := &outcomes[i]
			if o.err != nil {
				continue
			}
			b := o.backend
			// sort the results, if requested
			results := sorter(o.results)
			var numResults int
			if flagSearchInFilenames {
				selected := selectFilenames(results, flagLimit)
				for _, res := range selected {
					fmt.Printf(
						"%s:%s:%s (%s)\n\n",
						b.Name(),
						textBold.Sprint(toAnsiURL(res.RepoURL, repoNameFromRes(res))),
						textBold.Sprint(toAnsiURL(res.FileURL, res.Path)),
						textBold.Sprint(res.Branch),
					)
				}
				numResults = len(selected)
			} else {
				selected := selectResults(results, flagLimit, flagMatchFilename)
				for _, res := range selected {
					var before, after string
					for idx, line := range res.Context.Before {
						before += fmt.Sprintf("%d: %s\n", res.Lineno-(len(res.Context.Before)-idx), line)
					}
					for idx, line := range res.Context.After {
						after += fmt.Sprintf("%d: %s\n", res.Lineno+idx+1, line)
					}
					if len(res.Context.After) > 0 {
						after = "\n" + after
					}
					head, match, tail := splitHighlight(res.Line, res.Highlight)
					fmt.Printf(
						"%s:%s:%s (%s)\n\n%s%s: %s%s\n\n",
						res.Backend,
						textBold.Sprint(toAnsiURL(res.RepoURL, repoNameFromRes(res))),
						textBold.Sprint(toAnsiURL(res.FileURL, res.Path)),
						textBold.Sprint(res.Branch),
						before,
						textBoldGreen.Sprint(res.Lineno),
						head+textBoldRed.Sprint(match)+tail,
						after,
					)
				}
				numResults = len(selected)
			}
			o.printed = numResults
			totalResults += numResults
		}
		totalTime := time.Since(searchStart)
		if flagStats {
			for _, o := range outcomes {
				if o.err != nil {
					fmt.Fprintf(os.Stderr, "Failed on %q after %s\n", o.backend.Name(), o.duration)
					continue
				}
				fmt.Fprintf(os.Stderr, "Got %d results on %q in %s\n", o.printed, o.backend.Name(), o.duration)
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

func toAnsiURL(url, text string) string {
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
		logrus.Fatalf("Failed to execute command: %v", err)
	}
}
