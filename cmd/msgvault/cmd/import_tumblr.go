package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/tumblr"
)

type tumblrCLIOptions struct {
	Blog              string
	NoDefaultIdentity bool
}

func newImportTumblrCmd() *cobra.Command {
	var opts tumblrCLIOptions
	cmd := &cobra.Command{
		Use:   "import-tumblr --blog <blog-name> <folder-or-zip>",
		Short: "Import a Tumblr conversation export",
		Long: `Import one blog's HTML conversations and supplied local messaging media.
Pass the blog name explicitly; stable owner IDs reuse the source after renames.
Files must be on the daemon host. TEXT and POSTREF follow observed export
structure; IMAGE and standard HTML media have synthetic coverage only.
No links are fetched.

Replay adds missing observations and fills missing media, preserving archived
text and dates. Indistinguishable repeated messages have ambiguous overlap.
Missing files do not establish deletion, retention or link expiry.
Limits: 1,000,000 ZIP or visited entries; 128 MiB ZIP metadata or retained
path bytes; 64 MiB per HTML file and 100 MiB per media file.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
			}
			return runImportTumblr(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.Blog, "blog", "", "exporting blog name (required)")
	cmd.Flags().BoolVar(&opts.NoDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	_ = cmd.MarkFlagRequired("blog")
	return cmd
}
func runImportTumblr(cmd *cobra.Command, input string, opts tumblrCLIOptions) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Re-run to continue.")
	defer stop()
	s, importErr := tumblr.Import(ctx, st, input, tumblr.ImportOptions{Blog: opts.Blog, AttachmentsDir: state.cfg.AttachmentsDir(), Logger: state.logger})
	// Identity and cache maintenance must include writes committed before cancellation.
	maintenanceCtx := context.WithoutCancel(ctx)
	if !opts.NoDefaultIdentity && s.OwnerKey != "" {
		confirmDefaultIdentity(maintenanceCtx, cmd.OutOrStdout(), st, s.SourceID, opts.Blog, s.OwnerKey, "export-owner", state.logger)
	}
	err = errors.Join(importErr, runExportPostImportMigrations(maintenanceCtx, cmd.OutOrStdout(), st, s.SourceID, true, state))
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Files: %d conversation reads attempted, %d skipped; conversations: %d imported, %d for other blogs\n", s.Files, s.FilesSkipped, s.Conversations, s.ForeignConversations)
	_, _ = fmt.Fprintf(out, "Messages: %d added, %d present, %d unsupported, %d malformed, %d undated, %d date conflicts; %d unrecognized sections\n", s.MessagesAdded, s.MessagesPresent, s.Unsupported, s.Malformed, s.Undated, s.ConflictingDates, s.UnknownBlocks)
	if len(s.UnsupportedTypes) > 0 {
		_, _ = fmt.Fprintf(out, "Unsupported types: %v\n", s.UnsupportedTypes)
	}
	_, _ = fmt.Fprintf(out, "Media this run: %d saved, %d missing local files, %d remote-only, %d invalid, %d oversized\n", s.MediaStored, s.MediaMissing, s.MediaRemote, s.MediaInvalid, s.MediaOversized)
	if s.SourceID != 0 {
		err = errors.Join(err, rebuildCacheAfterWrite(maintenanceCtx, state.cfg.DatabaseDSN(), state))
	}
	return err
}
func init() { rootCmd.AddCommand(newImportTumblrCmd()) }
