package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/fbmessenger"
)

type instagramCLIOptions struct {
	Account, SelfName string
	Limit             int
}

func newImportInstagramCmd() *cobra.Command {
	var opts instagramCLIOptions
	cmd := &cobra.Command{
		Use:   "import-instagram <export-dir>",
		Short: "Import Instagram account inbox JSON",
		Long: `Import retained Instagram inbox messages and available media from an account download.

Supported layout: your_instagram_activity/messages/inbox/<thread>/message_<N>.json.
HTML, ZIP and other layouts are unsupported. Extract the JSON download first.
Display names are not verified handles; identical names within
one account merge. Both --account and --self-name are required.

Each run rereads the export, preserving duplicates across export files and repairing
reactions or missing media. Edited messages, changed sender display names and
changed conversation paths cannot be matched to earlier imports.
Deleted, unsent, encrypted and unexported history cannot be recovered.

Example:
  msgvault import-instagram --account orbit.example --self-name "Nova Example" ./export
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
			}
			return runImportInstagram(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.Account, "account", "", "Instagram account handle (required; @ is optional)")
	cmd.Flags().StringVar(&opts.SelfName, "self-name", "", "exact decoded export display name for sent messages")
	cmd.Flags().IntVar(&opts.Limit, "limit", 0, "maximum new messages to add (0 imports all)")
	_ = cmd.MarkFlagRequired("account")
	_ = cmd.MarkFlagRequired("self-name")
	return cmd
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
	summary, importErr := fbmessenger.ImportDYI(ctx, st, fbmessenger.ImportOptions{Source: "instagram", RootDir: exportDir, Account: opts.Account, SelfName: opts.SelfName, Limit: opts.Limit, AttachmentsDir: state.cfg.AttachmentsDir()})
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
	if summary.MessagesProcessed > summary.RecordsUnsupported && summary.FromMeCount == 0 {
		_, _ = fmt.Fprintln(out, "No sender exactly matched --self-name. Re-run with the exact exported display name to repair sent/received direction.")
	}
	if summary.LimitReached {
		_, _ = fmt.Fprintln(out, "Import paused at --limit. Re-run to add more messages.")
	}
	if summary.Incomplete {
		_, _ = fmt.Fprintln(out, "Incomplete coverage: unsupported records or files were encountered.")
	}
	_, _ = fmt.Fprintln(out, "Coverage: inbox JSON only; deleted, unsent, encrypted and unexported history cannot be recovered.")
	return errors.Join(importErr, err)
}

func init() { rootCmd.AddCommand(newImportInstagramCmd()) }
