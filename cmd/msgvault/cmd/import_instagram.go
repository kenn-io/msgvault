package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/fbmessenger"
)

type instagramCLIOptions struct {
	Account, SelfName, Format string
	Limit                     int
}

func runImportInstagram(cmd *cobra.Command, exportDir string, opts instagramCLIOptions) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Re-run the export to continue.")
	defer stop()
	summary, importErr := fbmessenger.ImportDYI(ctx, st, fbmessenger.ImportOptions{Source: "instagram", RootDir: exportDir, Account: opts.Account, SelfName: opts.SelfName, Format: opts.Format, Limit: opts.Limit, AttachmentsDir: state.cfg.AttachmentsDir()})
	// Source migrations and cache refresh must finish after a partial commit,
	// even when cancellation interrupted the importer. No display-name identity
	// is confirmed: --self-name describes direction only.
	cleanupCtx := context.WithoutCancel(ctx)
	if summary.SourceID != 0 {
		err = runPostSourceCreateMigrationsForInvocation(cleanupCtx, st, state)
		err = errors.Join(err, rebuildCacheAfterWrite(cleanupCtx, state.cfg.DatabaseDSN(), state))
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Instagram: %d threads processed; %d records supported, %d skipped; %d new messages; %d sent records.\n", summary.ThreadsProcessed, summary.MessagesProcessed-summary.RecordsUnsupported, summary.RecordsUnsupported, summary.MessagesAdded, summary.FromMeCount)
	_, _ = fmt.Fprintf(out, "Media: %d stored, %d missing, %d rejected. Files skipped: %d.\n", summary.AttachmentsStored, summary.MediaMissing, summary.MediaRejected, summary.FilesSkipped)
	if importErr == nil && !summary.LimitReached && (!summary.Incomplete || (summary.FilesSkipped == 0 && summary.RecordsUnsupported == 0 && summary.Errors == 0 && (summary.MediaMissing > 0 || summary.MediaRejected > 0))) && summary.MessagesProcessed > summary.RecordsUnsupported && summary.FromMeCount == 0 {
		_, _ = fmt.Fprintln(out, "No sender exactly matched --self-name. Re-run with the exact exported display name to repair sent/received direction.")
	}
	if summary.LimitReached {
		_, _ = fmt.Fprintln(out, "Import paused at --limit. Re-run to add more messages.")
	}
	if summary.Incomplete {
		_, _ = fmt.Fprintln(out, "Incomplete coverage: some content could not be imported.")
	}
	return errors.Join(importErr, err)
}
