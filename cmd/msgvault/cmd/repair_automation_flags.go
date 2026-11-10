package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

func newRepairAutomationFlagsCmd() *cobra.Command {
	var apply bool
	var batchSize int
	cmd := &cobra.Command{
		Use:   "repair-automation-flags [--apply]",
		Short: "Repair archived email automation flags from raw MIME",
		Long: `Re-derive automation flags for archived email messages from their stored raw MIME.

The default is a dry run: it reports the values that would change without
modifying the archive. Pass --apply to write the repaired values. This command
works entirely offline and never contacts a provider.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			return runRepairAutomationFlagsLocal(cmd, apply, batchSize)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write repaired automation flags to the archive")
	cmd.Flags().IntVar(&batchSize, "batch-size", 500, "messages per transaction (1–5000)")
	return cmd
}

func runRepairAutomationFlagsLocal(cmd *cobra.Command, apply bool, batchSize int) (repairErr error) {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	st, cleanup, err := openRepairStoreForInvocation(cmd.Context(), state, apply)
	if err != nil {
		return fmt.Errorf("open archive for automation flags repair: %w", err)
	}
	defer cleanup()
	if apply {
		// Durable revision drift also covers partial commits and a failed prior
		// refresh. The existing helper ignores cancellation and skips current caches.
		defer func() {
			repairErr = errors.Join(repairErr, rebuildCacheAfterWrite(cmd.Context(), cfg.DatabaseDSN(), state))
		}()
	}

	summary, err := st.RepairAutomationFlags(cmd.Context(), store.AutomationRepairOptions{Apply: apply, BatchSize: batchSize}, nil)
	if err != nil {
		return fmt.Errorf("repair automation flags: %w", err)
	}

	mode := "dry run"
	if apply {
		mode = "applied"
	}
	countLabel := "would_write"
	if apply {
		countLabel = "written"
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(),
		"Automation flag repair %s: scanned=%d %s=%d bulk=%d automated=%d unreadable=%d remaining=%d\n",
		mode, summary.Scanned, countLabel, summary.Written, summary.Bulk, summary.Automated, summary.Unreadable, summary.Remaining); err != nil {
		return fmt.Errorf("write automation repair summary: %w", err)
	}
	if !apply && summary.Written > 0 {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(),
			"Dry run: no rows were modified. Re-run with --apply to write repairs."); err != nil {
			return fmt.Errorf("write automation flags repair dry-run guidance: %w", err)
		}
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newRepairAutomationFlagsCmd())
}
