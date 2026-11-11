package cmd

import (
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"
	msgsync "go.kenn.io/msgvault/internal/sync"
)

var acquisitionStateLabels = map[string]string{
	msgsync.AcquisitionActive:        "still failing",
	msgsync.AcquisitionResolved:      "fixed",
	msgsync.AcquisitionHistorical:    "retired",
	msgsync.AcquisitionIndeterminate: "unknown",
}

func init() { rootCmd.AddCommand(newDiagnoseAcquisitionCmd()) }

func newDiagnoseAcquisitionCmd() *cobra.Command {
	var sourceID int64
	var jsonOut, all bool
	command := &cobra.Command{
		Use:   "diagnose-acquisition --source-id ID",
		Short: "List Gmail acquisition failures",
		Long:  "List Gmail acquisition failures that sync still retries in the local archive. Use --all to include fixed, retired, and unknown failures, and --local when a remote is configured.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if IsRemoteMode(invocationFromContext(ctx)) {
				return errors.New("diagnose-acquisition requires a local archive; use --local when remote.url is configured")
			}
			st, cleanup, err := openReadOnlyStoreForInvocation(ctx, invocationFromContext(ctx))
			if err != nil {
				return err
			}
			defer cleanup()
			source, err := st.GetSourceByIDContext(ctx, sourceID)
			if err != nil {
				return err
			}
			if source.SourceType != "" && source.SourceType != sourceTypeGmail {
				return errors.New("acquisition diagnostic requires a Gmail source")
			}
			if source.MergedIntoSourceID != 0 {
				return fmt.Errorf("source %d was merged into source %d and cannot sync", sourceID, source.MergedIntoSourceID)
			}
			items, err := msgsync.DiagnoseAcquisitionContext(ctx, st, sourceID)
			if err != nil {
				return err
			}
			if !all {
				items = slices.DeleteFunc(items, func(item msgsync.AcquisitionDiagnosticItem) bool {
					return item.State != msgsync.AcquisitionActive
				})
			}
			if jsonOut {
				return printJSONTo(cmd.OutOrStdout(), items)
			}
			if len(items) == 0 {
				empty := "No pending acquisition failures."
				if all {
					empty = "No recorded acquisition failures."
				}
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), empty); err != nil {
					return fmt.Errorf("write acquisition diagnostic: %w", err)
				}
				return nil
			}
			for _, item := range items {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", item.SourceMessageID, acquisitionStateLabels[item.State], msgsync.AcquisitionFailureDescription(item.ErrorKind)); err != nil {
					return fmt.Errorf("write acquisition diagnostic: %w", err)
				}
			}
			return nil
		},
	}
	command.Flags().Int64Var(&sourceID, "source-id", 0, "Gmail source ID (required)")
	command.Flags().BoolVar(&all, "all", false, "Include fixed, retired, and unknown failures")
	command.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON")
	_ = command.MarkFlagRequired("source-id")
	return command
}
