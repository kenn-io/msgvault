package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/remoteimage"
)

func configuredRemoteImageFetcher() *remoteimage.Fetcher {
	if cfg == nil || !cfg.Sync.ArchiveRemoteImages {
		return nil
	}
	return remoteimage.NewFetcher()
}

func newArchiveRemoteImagesCmd() *cobra.Command {
	var allowTracking bool
	var sourceID int64
	var limit int
	command := &cobra.Command{
		Use:   "archive-remote-images",
		Short: "Archive remote images in existing email (requires tracking consent)",
		Long:  "Download remote img src images from archived email for offline viewing.\n\nDownloading can activate tracking pixels and disclose the archive server's IP\naddress to senders. This command always requires --allow-tracking. It does not\nenable automatic archiving, change original messages, or re-fetch stored images.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !allowTracking {
				return errors.New("remote image downloads can activate tracking pixels; pass --allow-tracking to consent")
			}
			if limit < 0 || sourceID < 0 {
				return errors.New("source ID and limit must not be negative")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			st, cleanup, err := openWritableStoreAndInitForIngest()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted.")
			defer stop()
			result, err := remoteimage.NewFetcher().Backfill(ctx, st, cfg.AttachmentsDir(), sourceID, limit, logger)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Messages: %d\nDownloaded: %d images\nAlready archived: %d images\nErrors: %d\n", result.Messages, result.Downloaded, result.Reused, result.Errors)
			if result.Errors > 0 {
				err = errors.Join(err, fmt.Errorf("%d remote image errors; successful downloads were preserved", result.Errors))
			}
			// Attachment-only changes do not advance message or sync watermarks.
			if result.Downloaded > 0 || result.Reused > 0 {
				err = errors.Join(err, st.AdvanceDerivedDataRevision())
			}
			// Partial success and cancellation can still leave new attachments.
			return errors.Join(err, rebuildCacheAfterWrite(cfg.DatabaseDSN()))
		},
	}
	command.Flags().BoolVar(&allowTracking, "allow-tracking", false, "Consent to sender-controlled image requests and tracking pixels")
	command.Flags().Int64Var(&sourceID, "source-id", 0, "Archive images for one source ID (default: all email sources)")
	command.Flags().IntVar(&limit, "limit", 0, "Maximum messages to scan (0 = unlimited)")
	return command
}

func init() { rootCmd.AddCommand(newArchiveRemoteImagesCmd()) }
