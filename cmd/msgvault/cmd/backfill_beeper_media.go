package cmd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/beeper"
)

var backfillBeeperMediaAccounts []string

func newBackfillBeeperMediaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backfill-beeper-media",
		Short: "Retry pending Beeper attachment downloads",
		Long: `Retry eligible Beeper attachment downloads.

This command retries unfinished downloads and policy exclusions that are now
allowed, such as media whose configured size cap was raised. It re-fetches
eligible messages from Beeper Desktop and downloads their media into the
attachment store. Already-downloaded attachments are content-addressed and
skipped.

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
					return rebuildCacheAfterWrite(dbPath)
				}
				if err != nil {
					return errors.Join(
						fmt.Errorf("beeper media backfill failed for %s: %w", accountID, err),
						rebuildCacheAfterWrite(dbPath),
					)
				}
				writeBeeperMediaBackfillSummary(cmd.OutOrStdout(), accountID, sum)
				if sum.Errors > 0 {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %d errors — see sync run items; re-run to retry\n", sum.Errors)
				}
			}

			return rebuildCacheAfterWrite(dbPath)
		},
	}
	cmd.Flags().StringArrayVar(&backfillBeeperMediaAccounts, "account", nil, "Beeper accountID to backfill (repeatable; default: all registered accounts)")
	return cmd
}

func writeBeeperMediaBackfillSummary(out io.Writer, accountID string, sum *beeper.ImportSummary) {
	_, _ = fmt.Fprintf(out,
		"%s: %d messages checked, %d attachments downloaded, %d still pending, %d skipped by policy (%s)\n",
		accountID, sum.MessagesProcessed, sum.AttachmentsDownloaded, sum.AttachmentsPending,
		sum.AttachmentsSkipped, sum.Duration.Round(time.Second))
}

func init() {
	rootCmd.AddCommand(newBackfillBeeperMediaCmd())
}
