package cmd

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
)

var deleteDedupedCmd = &cobra.Command{
	Use:   "delete-deduped",
	Short: "Permanently delete dedup-hidden messages from the local archive",
	Long: `Permanently delete dedup-hidden messages from the local archive. This is
the third rung of the safety progression: scan -> hide -> local hard
delete -> remote delete. Each rung is a separate, explicit user action.

Use --batch <id> to delete rows hidden by a specific dedup batch.
Use --all-hidden to delete every dedup-hidden row regardless of batch.

Deleted rows cannot be recovered with --undo. Pending remote-deletion
manifests still reference Gmail/IMAP message IDs and remain valid
after a local delete.

Parquet analytics and the vector index may contain stale entries for
deleted rows until rebuilt; the rebuild commands are separate. Run
'msgvault build-cache --full-rebuild' for parquet analytics and
'msgvault embeddings build --full-rebuild' for the vector index.`,
	RunE: runDeleteDeduped,
}

var (
	deleteDedupedBatchIDs  []string
	deleteDedupedAllHidden bool
	deleteDedupedNoBackup  bool
	deleteDedupedYes       bool
)

func runDeleteDeduped(cmd *cobra.Command, _ []string) error {
	if len(deleteDedupedBatchIDs) == 0 && !deleteDedupedAllHidden {
		return usageErr(cmd, errors.New("must specify --batch or --all-hidden"))
	}

	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	req := daemonclient.CLIDeleteDedupedRequest{
		BatchIDs:  append([]string(nil), deleteDedupedBatchIDs...),
		AllHidden: deleteDedupedAllHidden,
		NoBackup:  deleteDedupedNoBackup,
	}
	plan, err := st.PlanCLIDeleteDeduped(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("plan delete-deduped: %w", err)
	}

	printDeleteDedupedPlan(cmd, plan)
	if plan.Total == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Nothing to delete.")
		return nil
	}

	// --all-hidden always prompts, even when --yes is set; spec rung 03 invariant.
	// Mode picks how EOF is handled: AllHidden treats closed stdin as a contract
	// violation (must not be silently bypassed), YesNo treats it as cancel.
	if !deleteDedupedYes || deleteDedupedAllHidden {
		mode := ConfirmModeYesNo
		if deleteDedupedAllHidden {
			mode = ConfirmModeAllHidden
		}
		ok, err := confirmDestructive(cmd.InOrStdin(), cmd.OutOrStdout(), mode)
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
			return nil
		}
	}

	// Note: parquet analytics and the vector index may contain entries
	// for deleted rows; the post-run summary recommends rebuilding each
	// separately ('build-cache --full-rebuild' and
	// 'embeddings build --full-rebuild').

	expectedTotal := plan.Total
	expectedBatchCount := plan.BatchCount
	req.ExpectedTotal = &expectedTotal
	req.ExpectedBatchCount = &expectedBatchCount
	req.ExpectedBatches = append([]daemonclient.CLIDeleteDedupedBatch{}, plan.Batches...)
	executed, err := st.ExecuteCLIDeleteDeduped(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("delete deduped: %w", err)
	}

	out := cmd.OutOrStdout()
	if !deleteDedupedNoBackup && executed.BackupPath != "" {
		_, _ = fmt.Fprintf(out, "Backing up database to %s...\n", filepath.Base(executed.BackupPath))
	}
	_, _ = fmt.Fprintf(out, "\nDeleted %d message(s) from %d batch(es).\n\n", executed.Deleted, executed.BatchCount)
	_, _ = fmt.Fprintln(out, "Caches may have stale entries; rebuild each separately:")
	_, _ = fmt.Fprintln(out, "  'msgvault build-cache --full-rebuild'        (parquet analytics)")
	_, _ = fmt.Fprintln(out, "  'msgvault embeddings build --full-rebuild'   (vector index, if enabled)")

	return nil
}

func printDeleteDedupedPlan(cmd *cobra.Command, plan *daemonclient.CLIDeleteDedupedPlan) {
	if plan == nil {
		plan = &daemonclient.CLIDeleteDedupedPlan{}
	}
	out := cmd.OutOrStdout()
	if deleteDedupedAllHidden {
		_, _ = fmt.Fprintf(out, "Will permanently delete %d hidden message(s) from %d distinct batch(es).\n",
			plan.Total, plan.BatchCount)
		return
	}

	_, _ = fmt.Fprintf(out, "Will permanently delete %d hidden message(s) from %d batch(es):\n",
		plan.Total, len(deleteDedupedBatchIDs))
	for _, batch := range plan.Batches {
		_, _ = fmt.Fprintf(out, "  %s: %d row(s)\n", batch.ID, batch.Count)
	}
}

func init() {
	rootCmd.AddCommand(deleteDedupedCmd)
	deleteDedupedCmd.Flags().StringArrayVar(&deleteDedupedBatchIDs, "batch", nil,
		"Delete rows hidden by this batch ID (repeat for multiple batches)")
	deleteDedupedCmd.Flags().BoolVar(&deleteDedupedAllHidden, "all-hidden", false,
		"Delete every dedup-hidden row regardless of batch")
	deleteDedupedCmd.MarkFlagsMutuallyExclusive("batch", "all-hidden")
	deleteDedupedCmd.Flags().BoolVar(&deleteDedupedNoBackup, "no-backup", false,
		"Skip database backup before deleting")
	deleteDedupedCmd.Flags().BoolVarP(&deleteDedupedYes, "yes", "y", false,
		"Skip confirmation prompt")
}
