package cmd

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
)

// setupStatusDeps isolates the report from the process so tests can drive it
// from a temp config and a fixed environment.
type setupStatusDeps struct {
	config      func() *config.Config
	environment func(*cobra.Command, *config.Config) setupEnvironment
}

func defaultSetupStatusDeps() setupStatusDeps {
	return setupStatusDeps{
		config: func() *config.Config { return cfg },
		environment: func(command *cobra.Command, loaded *config.Config) setupEnvironment {
			return setupEnvironment{
				lookupEnv:  os.LookupEnv,
				fileExists: defaultFileExists,
				consent:    readSetupConsentState(command.Context(), loaded),
			}
		},
	}
}

func newSetupStatusCommand(deps setupStatusDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Report which retrieval and people lanes are on, with provider, model, and the next step",
		Long: `Report every optional lane: text search, semantic people search, visual
attachment search, document extraction and vectors, the people sweep, the
activity projection, and the chat media policy. For each lane the report
names the provider and model, whether it is on, why it is off, whether its
consent is recorded, and the exact command that turns it on.

The report reads config.toml, the process environment, and the local
archive's consent records. It never contacts a provider.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			loaded := deps.config()
			if loaded == nil {
				return errors.New("configuration is unavailable")
			}
			report := buildLaneReport(loaded, deps.environment(command, loaded))
			return writeLaneReport(command.OutOrStdout(), report, jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}
