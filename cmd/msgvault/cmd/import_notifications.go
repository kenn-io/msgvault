package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"
)

const flagNotifyImports = "notify-imports"

type importResultContextKey struct{}

type importCommandResult struct {
	ImportRunIDs []string `json:"import_run_ids"`
	Completed    bool     `json:"completed"`
}

func addImportNotificationFlag(command *cobra.Command) {
	command.Flags().Bool(flagNotifyImports, false, "Notify subscribers of newly imported messages (also requires the daemon import opt-in)")
	command.Flags().Bool(flagJSON, false, "Output logical import run IDs and completion as JSON; progress goes to stderr")
	run := command.RunE
	command.RunE = func(command *cobra.Command, args []string) error {
		// The owner daemon executes the native command. Its proxy streams the
		// resulting stdout unchanged rather than producing a second result.
		if !isDaemonCLISubprocess() {
			return run(command, args)
		}
		result := &importCommandResult{ImportRunIDs: []string{}, Completed: true}
		originalContext := command.Context()
		command.SetContext(context.WithValue(originalContext, importResultContextKey{}, result))
		defer command.SetContext(originalContext)
		output := command.OutOrStdout()
		asJSON, _ := command.Flags().GetBool(flagJSON)
		if asJSON {
			command.SetOut(command.ErrOrStderr())
			defer command.SetOut(output)
		}
		runErr := run(command, args)
		result.Completed = result.Completed && runErr == nil
		if asJSON {
			return errors.Join(runErr, printJSONTo(output, result))
		}
		for _, id := range result.ImportRunIDs {
			if _, err := fmt.Fprintf(output, "Import run: %s\n", id); err != nil {
				return errors.Join(runErr, err)
			}
		}
		return runErr
	}
}

// Direct native command invocations without a registered flag retain the
// default silent behavior. Cobra validates the public boolean flag syntax.
func importNotificationsRequested(command *cobra.Command) bool {
	requested, _ := command.Flags().GetBool(flagNotifyImports)
	return requested
}

func recordImportRun(command *cobra.Command, id string) {
	result, _ := command.Context().Value(importResultContextKey{}).(*importCommandResult)
	if result != nil && id != "" && !slices.Contains(result.ImportRunIDs, id) {
		result.ImportRunIDs = append(result.ImportRunIDs, id)
	}
}

func markImportCommandIncomplete(command *cobra.Command) {
	result, _ := command.Context().Value(importResultContextKey{}).(*importCommandResult)
	if result != nil {
		result.Completed = false
	}
}
