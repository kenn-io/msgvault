package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

var backfillBeeperMediaAccounts []string

func newBackfillBeeperMediaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backfill-beeper-media",
		Short: "Retry pending Beeper attachment downloads",
		Long: `Retry pending Beeper attachment downloads.

Attachments that failed to download during sync-beeper (Beeper Desktop asset
temporarily unavailable, over the size cap at the time, transient errors)
leave a pending marker. This command re-fetches those messages from Beeper
Desktop and downloads their media into the attachment store. Idempotent:
already-downloaded attachments are content-addressed and skipped.

Examples:
  msgvault backfill-beeper-media
  msgvault backfill-beeper-media --account signal`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}

			imp, accountIDs, dbPath, cleanup, err := openBeeperImporter(backfillBeeperMediaAccounts)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Stopping...")
			defer stop()

			for _, accountID := range accountIDs {
				opts := beeperImportOptions(accountID)
				opts.Progress = func(s string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+s) }
				sum, err := imp.BackfillMedia(ctx, opts)
				if ctx.Err() != nil {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run backfill-beeper-media to resume (idempotent).")
					rebuildCacheAfterWrite(dbPath)
					return nil
				}
				if err != nil {
					rebuildCacheAfterWrite(dbPath)
					return fmt.Errorf("beeper media backfill failed for %s: %w", accountID, err)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"%s: %d messages checked, %d attachments downloaded, %d still pending (%s)\n",
					accountID, sum.MessagesProcessed, sum.AttachmentsDownloaded, sum.AttachmentsPending,
					sum.Duration.Round(time.Second))
				if sum.Errors > 0 {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %d errors — see sync run items; re-run to retry\n", sum.Errors)
				}
			}

			rebuildCacheAfterWrite(dbPath)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&backfillBeeperMediaAccounts, "account", nil, "Beeper accountID to backfill (repeatable; default: all registered accounts)")
	return cmd
}

func init() {
	rootCmd.AddCommand(newBackfillBeeperMediaCmd())
}
