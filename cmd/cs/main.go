package main

import (
	"fmt"
	"strings"

	"github.com/fatih/color"
	"github.com/insomniacslk/codesearch/pkg/codesearch"
	"github.com/kirsle/configdir"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const progname = "cs"

var (
	globalConfig codesearch.Config

	configFile string
	flagDebug  bool

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

	searchCmd.PersistentFlags().StringVarP(&searchBackends, "backends", "b", "", "Comma-separated list of names of the backends to use. The names are defined in your configuration file. If specified, it overrides `default_backends` in the configuration file. \"all\" will use every backend")

	rootCmd.AddCommand(searchCmd)
}

func initConfig() {
	if configFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(configFile)
	} else {
		configDir := configdir.LocalConfig(progname)
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")
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

var searchCmd = &cobra.Command{
	Use:   "search",
	Short: "Search code in the specified backends",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		config := getConfig()
		var defaultBackends []string
		for _, b := range config.DefaultBackends {
			// Config.Validate has already ensured that there is either "all" or
			// individual backend names.
			if b == "all" {
				// clear out defaultBackends in case other backends were
				// appended
				defaultBackends = make([]string, 0, len(config.Backends))
				for name := range config.Backends {
					defaultBackends = append(defaultBackends, name)
				}
				break
			} else {
				defaultBackends = append(defaultBackends, b)
			}
		}
		var backendNames []string
		if searchBackends == "" {
			// no override from command line, use default backends
			backendNames = defaultBackends
		} else {
			// backends overridden from command line
			backendNames = strings.Split(searchBackends, ",")
			// TODO remove duplicates
		}
		searchString := strings.Join(args, " ")
		fmt.Printf("Searching %q on %q\n", searchString, backendNames)
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
		for _, b := range backends {
			results, err := b.Search(searchString)
			if err != nil {
				logrus.Fatalf("Failed to search with backend %q: %v", b.Name(), err)
			}
			for _, res := range results {
				start, end := res.Highlight[0], res.Highlight[1]
				fmt.Printf(
					"%s:%s:%s\n%s: %s\n\n",
					res.Backend,
					textBold.Sprint(toAnsiURL(res.RepoURL, res.Owner+"/"+res.RepoName)),
					textBold.Sprint(toAnsiURL(res.FileURL, res.Path)),
					textBoldGreen.Sprint(res.Lineno),
					res.Line[:start]+textBoldRed.Sprint(res.Line[start:end])+res.Line[end:],
				)
			}
		}
	},
}

func toAnsiURL(url, text string) string {
	return fmt.Sprintf("\033]8;;%s\033\\%s\033]8;;\033\\", url, text)
}

var rootCmd = &cobra.Command{
	Use:   progname,
	Short: fmt.Sprintf("%q is a code searching tool inspired by Facebook's BigGrep.", progname),
	Long:  fmt.Sprintf("%s is a code searching tool inspired by Facebook's BigGrep. It can search across different code repository types (e.g. GitHub), and on the local filesystem using google/codesearch and ripgrep.", progname),
	Args:  cobra.MinimumNArgs(1),
	Run:   nil,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		logrus.Fatalf("Failed to execute command: %v", err)
	}
}
