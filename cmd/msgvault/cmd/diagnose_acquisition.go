package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
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
		Args:  cobra.NoArgs,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			inv := prepareInvocation(cmd)
			if isAgentMode(inv) {
				return errors.New("diagnose-acquisition is not available in agent-delegated mode")
			}
			var err error
			inv.cfg, err = config.LoadClientWithOverrides(inv.options.cfgFile, inv.options.homeDir, config.RuntimeOverrides{})
			return err
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sourceID <= 0 {
				return usageErr(cmd, errors.New("source ID must be positive"))
			}
			ctx := cmd.Context()
			cfg := invocationFromContext(ctx).cfg
			st, err := store.OpenReadOnlyContext(ctx, cfg.DatabaseDSN())
			if err != nil {
				return fmt.Errorf("open archive: %w", err)
			}
			defer func() { _ = st.Close() }()
			if err := st.ValidateSchemaContext(ctx); err != nil {
				return err
			}
			source, err := st.GetSourceByIDContext(ctx, sourceID)
			if err != nil {
				return err
			}
			if source.SourceType != "gmail" {
				return errors.New("acquisition diagnostic requires a Gmail source")
			}
			items, err := st.ListUnresolvedSyncItemErrors(ctx, sourceID)
			if err != nil {
				return err
			}
			active := make([]acquisitionFailure, 0, len(items))
			for _, item := range items {
				if item.IsAcquisitionReplayCandidate() {
					active = append(active, acquisitionFailure{item.SourceMessageID, item.Phase, item.ErrorKind})
				}
			}
			if jsonOut {
				return json.MarshalWrite(cmd.OutOrStdout(), active)
			}
			for _, item := range active {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", item.SourceMessageID, item.Phase, item.ErrorKind); err != nil {
					return err
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
