package cmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/groupme"
)

type groupmeCLIOptions struct {
	Me                string
	NoDefaultIdentity bool
}

func newImportGroupmeCmd() *cobra.Command {
	var opts groupmeCLIOptions
	cmd := &cobra.Command{
		Use: "import-groupme <export-dir>", Short: "Import a GroupMe JSON export",
		Long: `Import selected GroupMe conversations and direct messages from an extracted
JSON export. Select a conversation directory containing message.json, or its
parent directory. Find your account ID in a message you know you sent:
sender_id, corroborated by user_id. You can also use your own member user_id
in conversation.json. Reuse exactly that ID for later imports. Selected
incoming-only or empty conversations may omit your ID; msgvault does not
infer ownership from which IDs occur.

Messages, raw JSON, observed participants and supported local image/video media
are copied into the archive. URLs are never fetched. System records and unsupported
media are counted separately. Missing sender and timestamp values remain unknown.
Re-running keeps existing messages and adds missing messages and media.
Interrupted imports can safely restart.
Export coverage depends on the conversations selected and GroupMe retention.

Example:
  msgvault import-groupme ./groupme-export --me example-owner
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Me = strings.TrimSpace(opts.Me)
			if opts.Me == "" {
				return errors.New("--me <account-id> is required")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
			}
			return runImportGroupme(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.Me, "me", "", "your opaque GroupMe account ID (required, not a phone number)")
	cmd.Flags().BoolVar(&opts.NoDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
}

func runImportGroupme(cmd *cobra.Command, exportDir string, opts groupmeCLIOptions) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Finishing partial import...")
	defer stop()
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Importing selected GroupMe conversations from %s\n", exportDir)
	summary, importErr := groupme.Import(ctx, s, groupme.ImportOptions{Me: opts.Me, RootDir: exportDir, AttachmentsDir: state.cfg.AttachmentsDir(), Logger: state.logger})
	// Identity migrations and cache refresh must see writes made before cancellation.
	finalize := context.WithoutCancel(ctx)
	err = errors.Join(importErr, runExportPostImportMigrations(finalize, cmd.OutOrStdout(), s, summary.SourceID, opts.NoDefaultIdentity, state))
	if summary.SourceID != 0 {
		err = errors.Join(err, rebuildCacheAfterWrite(finalize, state.cfg.DatabaseDSN(), state))
	}
	if err != nil && summary.SourceID == 0 {
		return err
	}
	out := cmd.OutOrStdout()
	if err != nil {
		_, _ = fmt.Fprintln(out, "Partial import. Re-run to continue safely.")
	} else {
		_, _ = fmt.Fprintln(out, "Import complete.")
	}
	_, _ = fmt.Fprintf(out, "  Duration: %s\n  Conversations: %d\n  Messages: %d added, %d existing, %d unsupported, %d system skipped\n  Messages from --me: %d\n  Media: %d stored, %d missing, %d oversized\n  Unsupported attachment forms: %d\n", summary.Duration.Round(time.Millisecond), summary.ChatsProcessed, summary.MessagesAdded, summary.MessagesSkipped, summary.UnsupportedEntriesSkipped, summary.ServiceMessagesSkipped, summary.MessagesFromMe, summary.AttachmentsStored, summary.AttachmentsMissing, summary.AttachmentsOversized, summary.AttachmentsUnsupported)
	if !summary.Oldest.IsZero() {
		_, _ = fmt.Fprintf(out, "  Known dates (UTC): %s to %s\n", summary.Oldest.Format(time.RFC3339), summary.Newest.Format(time.RFC3339))
	} else {
		_, _ = fmt.Fprintln(out, "  Known dates: none")
	}
	years := make([]int, 0, len(summary.Years))
	for year := range summary.Years {
		years = append(years, year)
	}
	sort.Ints(years)
	for _, year := range years {
		_, _ = fmt.Fprintf(out, "  Year %d (UTC): %d\n", year, summary.Years[year])
	}
	types := make([]string, 0, len(summary.Types))
	for kind := range summary.Types {
		types = append(types, kind)
	}
	sort.Strings(types)
	for _, kind := range types {
		_, _ = fmt.Fprintf(out, "  Type %s: %d\n", kind, summary.Types[kind])
	}
	return err
}

func init() { rootCmd.AddCommand(newImportGroupmeCmd()) }
