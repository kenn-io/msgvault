package cmd

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

type gcOptions struct {
	yes      bool
	noBackup bool
}

func newGCCmd() *cobra.Command {
	options := gcOptions{}
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Purge source-deleted messages and compact the SQLite archive",
		Long: `Permanently delete local archive rows already marked deleted from their
source, then compact the SQLite database. Active messages and rows hidden only
by deduplication are retained.

GC creates a point-in-time SQLite backup before deletion by default and asks
for confirmation. Use --yes for non-interactive confirmation or --no-backup to
explicitly opt out of the backup. This command never deletes remote messages.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				if !options.yes {
					confirmed, err := confirmDestructive(
						cmd.InOrStdin(), cmd.OutOrStdout(), ConfirmModeYesNo,
					)
					if err != nil {
						return err
					}
					if !confirmed {
						if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Aborted."); err != nil {
							return fmt.Errorf("write GC aborted summary: %w", err)
						}
						return nil
					}
					if err := cmd.Flags().Set("yes", "true"); err != nil {
						return fmt.Errorf("record GC confirmation: %w", err)
					}
				}
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			return runGCLocal(cmd, options)
		},
	}
	cmd.Flags().BoolVarP(&options.yes, "yes", "y", false, "skip confirmation prompt")
	cmd.Flags().BoolVar(&options.noBackup, "no-backup", false, "skip the pre-delete SQLite backup")
	return cmd
}

func runGCLocal(cmd *cobra.Command, options gcOptions) error {
	ctx := cmd.Context()
	st, cleanup, err := openWritableStoreAndInit()
	if err != nil {
		return err
	}
	defer cleanup()
	if st.IsPostgreSQL() {
		return store.ErrGCUnsupported
	}

	plan, err := st.PlanGCContext(ctx)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(
		out,
		"Source-deleted messages to purge: %d\nDedup-hidden messages retained: %d\n",
		plan.SourceDeleted,
		plan.DedupHiddenRetained,
	); err != nil {
		return fmt.Errorf("write GC plan: %w", err)
	}
	if plan.SourceDeleted == 0 {
		if _, err := fmt.Fprintln(out, "Nothing to purge."); err != nil {
			return fmt.Errorf("write GC empty plan: %w", err)
		}
		return nil
	}

	if !options.yes {
		confirmed, err := confirmDestructive(
			cmd.InOrStdin(), out, ConfirmModeYesNo,
		)
		if err != nil {
			return err
		}
		if !confirmed {
			if _, err := fmt.Fprintln(out, "Aborted."); err != nil {
				return fmt.Errorf("write GC aborted summary: %w", err)
			}
			return nil
		}
	}

	if !options.noBackup {
		dbPath, err := cfg.DatabasePath()
		if err != nil {
			return fmt.Errorf("resolve database path for GC backup: %w", err)
		}
		backupPath := fmt.Sprintf(
			"%s.gc-backup-%s",
			dbPath,
			time.Now().UTC().Format("20060102-150405"),
		)
		if _, err := fmt.Fprintf(
			out, "Backing up database to %s...\n", filepath.Base(backupPath),
		); err != nil {
			return fmt.Errorf("write GC backup path: %w", err)
		}
		if err := st.BackupDatabaseContext(ctx, backupPath); err != nil {
			return fmt.Errorf("backup database before GC: %w", err)
		}
	}

	deleted, err := st.ExecuteGCContext(ctx, plan)
	if err != nil {
		return fmt.Errorf("execute GC: %w", err)
	}
	if _, err := fmt.Fprintf(
		out, "Deleted %d source-deleted message(s).\n", deleted,
	); err != nil {
		return fmt.Errorf("write GC deletion summary: %w", err)
	}
	if err := st.VacuumContext(ctx); err != nil {
		return fmt.Errorf(
			"deleted %d source-deleted message(s), but SQLite compaction failed: %w",
			deleted,
			err,
		)
	}
	if _, err := fmt.Fprintln(out, "SQLite archive compacted."); err != nil {
		return fmt.Errorf("write GC compaction summary: %w", err)
	}
	if _, err := fmt.Fprintln(out,
		"Derived caches may contain deleted rows; rebuild each enabled cache:",
		"\n  msgvault build-cache --full-rebuild",
		"\n  msgvault embeddings build --full-rebuild",
	); err != nil {
		return fmt.Errorf("write GC cache guidance: %w", err)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newGCCmd())
}
