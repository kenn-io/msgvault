package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

func newRepairLabelsCmd() *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "repair-labels [identifier] [--apply]",
		Short: "Rebuild IMAP message labels from stored mailbox memberships",
		Long: `Rebuild each IMAP source's message labels from its stored
imap_message_memberships rows — the same rebuild a full mailbox enumeration
performs, run on demand instead of on every sync.

An add-only label merge, used when a sync's mailbox snapshot is incomplete or
a dedup match is not fully confirmed, never removes a label. If the message's
stored membership never changes again, no later sync revisits it, and a stray
label can persist. This command finds and removes it.

The default is a dry run: it reports what would change without modifying the
archive. Pass --apply to write the repaired labels. This command works
entirely offline and never contacts a provider.

Examples:
  msgvault repair-labels                    # all IMAP sources
  msgvault repair-labels you@example.com    # one source
  msgvault repair-labels --apply`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			only := ""
			if len(args) == 1 {
				only = strings.TrimSpace(args[0])
			}
			return runRepairLabelsLocal(cmd, only, apply)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write repaired labels to the archive")
	return cmd
}

func runRepairLabelsLocal(cmd *cobra.Command, only string, apply bool) error {
	// A dry run still needs a writable connection: RepairIMAPSourceLabels
	// plans by writing inside a transaction and rolling it back, the same
	// idiom the store package uses elsewhere for a dry-run plan (see
	// errAttributeDryRun) — a read-only connection would reject the writes
	// outright before rollback ever came into it.
	st, cleanup, err := openWritableStoreAndInit()
	if err != nil {
		return fmt.Errorf("open archive for label repair: %w", err)
	}
	defer cleanup()

	sources, err := st.ListSourcesContext(cmd.Context(), sourceTypeIMAP)
	if err != nil {
		return fmt.Errorf("list IMAP sources: %w", err)
	}

	var totalScanned, totalChanged int
	for _, src := range sources {
		if only != "" && !store.EqualIdentifier(src.Identifier, only) {
			continue
		}
		summary, err := st.RepairIMAPSourceLabels(cmd.Context(), src.ID, apply)
		if err != nil {
			return fmt.Errorf("repair labels for %s: %w", src.Identifier, err)
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s: scanned=%d changed=%d\n",
			src.Identifier, summary.Scanned, summary.Changed); err != nil {
			return fmt.Errorf("write label repair line: %w", err)
		}
		totalScanned += summary.Scanned
		totalChanged += summary.Changed
	}

	mode := "dry run"
	if apply {
		mode = "applied"
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(),
		"Label repair %s: scanned=%d changed=%d\n", mode, totalScanned, totalChanged); err != nil {
		return fmt.Errorf("write label repair summary: %w", err)
	}
	if !apply && totalChanged > 0 {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(),
			"Dry run: no rows were modified. Re-run with --apply to write repairs."); err != nil {
			return fmt.Errorf("write label repair dry-run guidance: %w", err)
		}
	}
	if apply && totalChanged > 0 {
		if err := rebuildCacheAfterWrite(cfg.DatabaseDSN()); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newRepairLabelsCmd())
}
