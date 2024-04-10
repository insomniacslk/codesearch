package main

import (
	_ "embed"
	"fmt"
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

	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(configExampleCmd)
}

func initConfig() {
	if configFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(configFile)
	} else {
		configDir := configdir.LocalConfig(progname)
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(configDir)

		viper.AutomaticEnv()

		if err := viper.ReadInConfig(); err != nil {
			if _, ok := err.(viper.ConfigFileNotFoundError); ok {
				// TODO create new config file
				logrus.Fatalf("Config file not found")
			} else {
				logrus.Fatalf("Failed to read config file: %v", err)
			}
		}
		config := getConfig()
		if err := viper.Unmarshal(&config); err != nil {
			logrus.Fatalf("Failed to unmarshal config: %v", err)
		}
		if err := config.Validate(); err != nil {
			logrus.Fatalf("Invalid config: %v", err)
		}
	}

	if flagDebug {
		logrus.SetLevel(logrus.DebugLevel)
	}
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
		sort := getSorter(flagSort)
		if sort == nil {
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
		type stat struct {
			name     string
			duration time.Duration
			results  int
		}
		stats := make([]stat, 0, len(backends))
		searchStart := time.Now()
		totalResults := 0
		for _, b := range backends {
			start := time.Now()
			results, err := b.Search(
				searchString,
				codesearch.WithLinesBefore(flagSearchContextBefore),
				codesearch.WithLinesAfter(flagSearchContextAfter),
				codesearch.WithCaseInsensitive(flagCaseInsensitive),
			)
			if err != nil {
				logrus.Fatalf("Failed to search with backend %q: %v", b.Name(), err)
			}
			st := stat{
				name:     b.Name(),
				duration: time.Since(start),
			}
			// sort the results, if requested
			results = sort(results)
			numResults := 0
			for idx, res := range results {
				if flagLimit > 0 && uint(idx) >= flagLimit {
					break
				}
				if flagSearchInFilenames {
					// we are searching the pattern in the file name
					if strings.Contains(strings.ToLower(res.Path), strings.ToLower(searchString)) {
						fmt.Printf(
							"%s:%s:%s (%s)\n\n",
							res.Backend,
							textBold.Sprint(toAnsiURL(res.RepoURL, res.Owner+"/"+res.RepoName)),
							textBold.Sprint(toAnsiURL(res.FileURL, res.Path)),
							textBold.Sprint(res.Branch),
						)
						numResults++
					}
				} else {
					// we are searching the pattern in the file content
					// get context lines
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
					// get start and end of highlight
					start, end := res.Highlight[0], res.Highlight[1]
					var repoName string
					if res.Owner != "" {
						repoName = res.Owner
						if res.RepoName != "" {
							repoName += "/"
						}
					}
					repoName += res.RepoName
					if flagMatchFilename != "" {
						if strings.Contains(strings.ToLower(res.Path), strings.ToLower(flagMatchFilename)) {
							// only show the result if the file name matches the
							// file pattern
							if !res.IsFilename {
								fmt.Printf(
									"%s:%s:%s (%s)\n\n%s%s: %s%s\n\n",
									res.Backend,
									textBold.Sprint(toAnsiURL(res.RepoURL, repoName)),
									textBold.Sprint(toAnsiURL(res.FileURL, res.Path)),
									textBold.Sprint(res.Branch),
									before,
									textBoldGreen.Sprint(res.Lineno),
									res.Line[:start]+textBoldRed.Sprint(res.Line[start:end])+res.Line[end:],
									after,
								)
								numResults++
							}
						}
					} else {
						// no file name pattern specified, so show all the
						// results
						if res.IsFilename {
							continue
						}
						fmt.Printf(
							"%s:%s:%s (%s)\n\n%s%s: %s%s\n\n",
							res.Backend,
							textBold.Sprint(toAnsiURL(res.RepoURL, repoName)),
							textBold.Sprint(toAnsiURL(res.FileURL, res.Path)),
							textBold.Sprint(res.Branch),
							before,
							textBoldGreen.Sprint(res.Lineno),
							res.Line[:start]+textBoldRed.Sprint(res.Line[start:end])+res.Line[end:],
							after,
						)
						numResults++
					}
				}
			}
			st.results = numResults
			stats = append(stats, st)
			totalResults += numResults
		}
		totalTime := time.Since(searchStart)
		if flagStats {
			for _, st := range stats {
				fmt.Fprintf(os.Stderr, "Got %d results on %q in %s\n", st.results, st.name, st.duration)
			}
		}
		fmt.Fprintf(os.Stderr, "Got %d total results in %s\n", totalResults, totalTime)
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
