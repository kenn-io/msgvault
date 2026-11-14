package cmd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/signalexport"
)

func newImportSignalExportCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "import-signal-export <export-dir>",
		Short: "Import a Signal readable export",
		Long: `Import standard messages and available local media from a native Signal
readable directory (metadata.json version 1, main.jsonl, optional files/).

Re-running adds missing history and retries missing media; first archived
text is kept. Unsupported items and missing media are counted.

Example:
  msgvault import-signal-export ./signal-export --account personal`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(account) == "" {
				return errors.New("--account requires a stable nonempty label")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
			}
			return runImportSignalExport(cmd, args[0], account)
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "stable account label (required; reuse for later exports)")
	return cmd
}

func runImportSignalExport(cmd *cobra.Command, root, account string) error {
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
	summary, err := signalexport.Import(ctx, st, signalexport.ImportOptions{
		RootDir:        root,
		Account:        account,
		AttachmentsDir: state.cfg.AttachmentsDir(),
	})
	if errors.Is(err, signalexport.ErrNewerFormat) {
		return err
	}
	// The source label is a scope, not evidence of a contact address. Only the
	// importer's actual Self frame can confirm an owner identifier.
	err = errors.Join(err, runExportPostImportMigrations(ctx, cmd.OutOrStdout(), st, summary.SourceID, true, state))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Messages: %d added, %d existing, %d changed existing (first snapshot retained)\n"+
			"Media: %d stored, %d missing\n"+
			"Records: %d empty, %d unrecognized\n",
		summary.MessagesAdded, summary.MessagesExisting, summary.MessagesChanged,
		summary.AttachmentsStored, summary.AttachmentsMissing,
		summary.EmptyFrames, summary.UnknownFrames,
	)
	for _, group := range []struct {
		name   string
		counts map[string]int64
	}{{"Skipped", summary.Skipped}, {"Omitted", summary.Omitted}, {"Missing media", summary.MediaMissing}} {
		for _, key := range slices.Sorted(maps.Keys(group.counts)) {
			if group.counts[key] > 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %d\n", group.name, signalExportReasonLabel(key), group.counts[key])
			}
		}
	}
	if summary.SourceID != 0 {
		// Cache publication must include committed messages after interruption.
		err = errors.Join(err, rebuildCacheAfterWrite(context.WithoutCancel(ctx), state.cfg.DatabaseDSN(), state))
	}
	if err != nil {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Import failed. Re-run to continue. Committed messages are preserved.")
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Import complete. Coverage is limited to supported records; omissions are shown above.")
	return nil
}

var signalExportReasonLabels = map[string]string{
	"sms":                     "SMS metadata",
	"expire_start_date":       "expiry start dates",
	"expires_in_ms":           "expiry durations",
	"ambiguous_message":       "ambiguous messages",
	"recipient_identity":      "unsupported chat recipient",
	"author_identity":         "unsupported message author",
	"self_identity":           "missing account identity",
	"revision":                "invalid edit history",
	"missing":                 "file not found",
	"index_limit":             "file scan limit reached",
	"invalid_hash":            "missing or invalid file hash",
	"contactMessage":          "contact cards",
	"stickerMessage":          "stickers",
	"remoteDeletedMessage":    "deleted messages",
	"updateMessage":           "chat updates",
	"paymentNotification":     "payments",
	"giftBadge":               "gifts",
	"viewOnceMessage":         "view-once messages",
	"directStoryReplyMessage": "story replies",
	"adminDeletedMessage":     "messages deleted by admins",
	"adHocCall":               "calls",
	"installedStickerPack":    "installed sticker packs",
	"hiddenStickerPack":       "hidden sticker packs",
	"favoriteStickerList":     "favorite stickers",
	"notificationProfile":     "notification profiles",
	"chatFolder":              "chat folders",
}

func signalExportReasonLabel(reason string) string {
	if label, ok := signalExportReasonLabels[reason]; ok {
		return label
	}
	return strings.ToLower(strings.ReplaceAll(reason, "_", " "))
}

func init() {
	rootCmd.AddCommand(newImportSignalExportCmd())
}
