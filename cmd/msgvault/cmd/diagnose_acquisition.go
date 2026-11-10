package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	msgsync "go.kenn.io/msgvault/internal/sync"
)

type acquisitionFailure struct {
	SourceMessageID string `json:"source_message_id"`
	Phase           string `json:"phase"`
	ErrorKind       string `json:"error_kind"`
}

func init() { rootCmd.AddCommand(newDiagnoseAcquisitionCmd()) }

func newDiagnoseAcquisitionCmd() *cobra.Command {
	var sourceID int64
	var jsonOut bool
	command := &cobra.Command{
		Use:   "diagnose-acquisition --source-id ID",
		Short: "List pending Gmail acquisition failures",
		Long:  "List pending Gmail acquisition failures in the local archive. Use --local when a remote is configured.",
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
			items, err := st.ListUnresolvedSyncItemErrors(ctx, sourceID)
			if err != nil {
				return err
			}
			active := make([]acquisitionFailure, 0, len(items))
			for _, item := range items {
				if msgsync.IsAcquisitionReplayCandidate(item) {
					active = append(active, acquisitionFailure{item.SourceMessageID, item.Phase, item.ErrorKind})
				}
			}
			if jsonOut {
				return printJSONTo(cmd.OutOrStdout(), active)
			}
			if len(active) == 0 {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), "No pending acquisition failures."); err != nil {
					return fmt.Errorf("write acquisition diagnostic: %w", err)
				}
				return nil
			}
			for _, item := range active {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", item.SourceMessageID, msgsync.AcquisitionFailureDescription(item.ErrorKind)); err != nil {
					return fmt.Errorf("write acquisition diagnostic: %w", err)
				}
			}
			return nil
		},
	}
	command.Flags().Int64Var(&sourceID, "source-id", 0, "Gmail source ID (required)")
	command.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON")
	_ = command.MarkFlagRequired("source-id")
	return command
}
