package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func init() { rootCmd.AddCommand(newDiagnoseAcquisitionCmd()) }

func newDiagnoseAcquisitionCmd() *cobra.Command {
	var opts store.AcquisitionDiagnosticOptions
	var jsonOut bool
	var timeout time.Duration
	command := &cobra.Command{
		Use:   "diagnose-acquisition --source-id ID",
		Short: "Show known Gmail download and file-saving status (read-only)",
		Long: `Read the selected Gmail source's local ledger, without starting the daemon,
syncing, repairing, or contacting Gmail. Uses the local database configuration,
including a configured PostgreSQL database; [remote].url is not used.
The absence of known active obligations does not prove archive completeness.`,
		Args: cobra.NoArgs,
		// The normal root startup creates directories and may write logs or
		// telemetry. This command only loads configuration and opens read-only.
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
			if timeout <= 0 || timeout > 5*time.Minute {
				return usageErr(cmd, errors.New("timeout must be positive and at most 5m"))
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			cfg := invocationFromContext(ctx).cfg
			st, err := store.OpenReadOnlyContext(ctx, cfg.DatabaseDSN())
			if err != nil {
				return fmt.Errorf("open acquisition diagnostic read-only: %w", err)
			}
			defer func() { _ = st.Close() }()
			opts.RemoteImagesEnabled = cfg.Sync.ArchiveRemoteImages
			report, err := st.DiagnoseAcquisitionContext(ctx, opts)
			if err != nil {
				return err
			}
			if jsonOut {
				return json.MarshalWrite(cmd.OutOrStdout(), report, json.Deterministic(true))
			}
			if _, err := fmt.Fprint(cmd.OutOrStdout(), report.String()); err != nil {
				return fmt.Errorf("write acquisition diagnostic: %w", err)
			}
			return nil
		},
	}
	command.Flags().Int64Var(&opts.SourceID, "source-id", 0, "Gmail source ID (required)")
	command.Flags().StringVar(&opts.State, "state", "active", "Detail state: active, resolved, historical, indeterminate")
	command.Flags().IntVar(&opts.Limit, "limit", 50, "Maximum message details, 1 to 1000")
	command.Flags().Int64Var(&opts.AfterID, "after-id", 0, "Continue the selected state's page after this outcome ID")
	command.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "Database diagnostic deadline, at most 5m")
	command.Flags().BoolVar(&jsonOut, "json", false, "Emit versioned JSON")
	_ = command.MarkFlagRequired("source-id")
	return command
}
