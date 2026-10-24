package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
)

func newImportICSCommand() *cobra.Command {
	var identifier string
	cmd := &cobra.Command{
		Use: "import-ics <file>", Short: "Import local ICS events", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(identifier) == "" {
				return errors.New("--identifier is required")
			}
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args, nil)
			}
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Stopping...")
			defer stop()
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			summary, importErr := importer.ImportICS(ctx, st, args[0], identifier)
			if importErr != nil {
				return errors.Join(importErr, rebuildCacheAfterWrite(state.cfg.DatabaseDSN(), state))
			}
			printICSImportSummary(cmd, summary)
			return rebuildCacheAfterWrite(state.cfg.DatabaseDSN(), state)
		},
	}
	cmd.Flags().StringVar(&identifier, "identifier", "", "Stable name for this calendar export (required)")
	return cmd
}
func init() { rootCmd.AddCommand(newImportICSCommand()) }

func printICSImportSummary(cmd *cobra.Command, summary *store.ICalendarImportSummary) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Events: %d updated, %d unchanged, %d unexpandable\n", summary.Changed, summary.Unchanged, summary.Unexpandable)
	for _, diagnostic := range summary.Diagnostics {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), diagnostic)
	}
}
