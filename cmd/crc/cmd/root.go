package cmd

import (
	"github.com/code-ready/crc/pkg/crc/output"
	"github.com/spf13/cobra"

	cmdConfig "github.com/code-ready/crc/cmd/crc/cmd/config"

	"github.com/code-ready/crc/pkg/crc/config"
	"github.com/code-ready/crc/pkg/crc/constants"
	"github.com/code-ready/crc/pkg/crc/logging"
)

var rootCmd = &cobra.Command{
	Use:   commandName,
	Short: descriptionShort,
	Long:  descriptionLong,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		runPrerun()
	},
	Run: func(cmd *cobra.Command, args []string) {
		runRoot()
		cmd.Help()
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		runPostrun()
	},
}

func init() {
	if err := constants.EnsureBaseDirExists(); err != nil {
		logging.Fatal(err.Error())
	}
	if err := config.EnsureConfigFileExists(); err != nil {
		logging.Fatal(err.Error())
	}
	config.InitViper()

	setConfigDefaults()

	// subcommands
	rootCmd.AddCommand(cmdConfig.ConfigCmd)

	rootCmd.PersistentFlags().StringVar(&logging.LogLevel, "log-level", constants.DefaultLogLevel, "log level (e.g. \"debug | info | warn | error\")")
}

func runPrerun() {
	output.OutF("%s - %s\n", commandName, descriptionShort)
	// Setting up logrus
	logging.InitLogrus(logging.LogLevel)
	logging.SetupFileHook()
}

func runPostrun() {
	logging.CloseLogging()
}

func runRoot() {
	output.Out("No command given")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		logging.Fatal(err)
	}
}

func setConfigDefaults() {
	for _, setting := range cmdConfig.SettingsList {
		config.SetDefault(setting.Name, setting.DefaultValue)
	}
}
