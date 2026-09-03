package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

func newRepairListIDsCmd() *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "repair-list-ids [--apply]",
		Short: "Repair archived email List-Id values from raw MIME",
		Long: `Re-derive List-Id values for archived email messages from their stored raw MIME.

The default is a dry run: it reports the values that would change without
modifying the archive. Pass --apply to write the repaired values. This command
works entirely offline and never contacts a provider.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			return runRepairListIDsLocal(cmd, apply)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write repaired List-Id values to the archive")
	return cmd
}

func runRepairListIDsLocal(cmd *cobra.Command, apply bool) error {
	var (
		st      *store.Store
		cleanup func()
		err     error
	)
	if apply {
		st, cleanup, err = openWritableStoreAndInit()
	} else {
		st, err = store.OpenReadOnly(cfg.DatabaseDSN())
		cleanup = func() { _ = st.Close() }
	}
	if err != nil {
		return fmt.Errorf("open archive for List-Id repair: %w", err)
	}
	defer cleanup()

	summary, err := st.RepairListIDs(cmd.Context(), store.ListIDRepairOptions{Apply: apply}, nil)
	if err != nil {
		return fmt.Errorf("repair List-Id values: %w", err)
	}

	mode := "dry run"
	if apply {
		mode = "applied"
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(),
		"List-Id repair %s: scanned=%d found=%d changed=%d undecodable=%d\n",
		mode, summary.Scanned, summary.Found, summary.Changed, summary.Undecodable); err != nil {
		return fmt.Errorf("write List-Id repair summary: %w", err)
	}
	if !apply && summary.Changed > 0 {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(),
			"Dry run: no rows were modified. Re-run with --apply to write repairs."); err != nil {
			return fmt.Errorf("write List-Id repair dry-run guidance: %w", err)
		}
	}
	if apply && summary.Changed > 0 {
		if err := rebuildCacheAfterWrite(cfg.DatabaseDSN()); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newRepairListIDsCmd())
}
