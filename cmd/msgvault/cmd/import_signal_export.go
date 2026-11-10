package cmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/signalexport"
)

func newImportSignalExportCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "import-signal-export <export-dir>",
		Short: "Import a native Signal readable export",
		Long: `Import standard messages and available local media from a native Signal
readable directory (metadata.json version 1, main.jsonl, optional files/).

--account is a stable label for one account. Reuse it for later exports of
that account. The export does not provide an owner phone or ACI.
Re-running adds missing history and retries missing media. First archived
text and raw messages remain unchanged, including after edits. Unsupported
items, nested features and missing media are counted. No history completeness
or runtime cross-platform compatibility is claimed.

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
	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Re-run to continue through deduplication.")
	defer stop()
	summary, err := signalexport.Import(ctx, st, signalexport.ImportOptions{
		RootDir:        root,
		Account:        account,
		AttachmentsDir: state.cfg.AttachmentsDir(),
		Logger:         state.logger,
	})
	// The source label is a scope, not evidence of a contact address. Only the
	// importer's actual Self frame can confirm an owner identifier.
	err = errors.Join(err, runExportPostImportMigrations(ctx, cmd.OutOrStdout(), st, summary.SourceID, true, state))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Messages: %d added, %d existing, %d changed existing (first snapshot retained)\n"+
			"Media: %d stored, %d missing\n"+
			"Frames: %d empty, %d unknown\n",
		summary.MessagesAdded, summary.MessagesExisting, summary.MessagesChanged,
		summary.AttachmentsStored, summary.AttachmentsMissing,
		summary.EmptyFrames, summary.UnknownFrames,
	)
	for _, group := range []struct {
		name   string
		counts map[string]int64
	}{{"Skipped", summary.Skipped}, {"Omitted", summary.Omitted}, {"Missing media", summary.MediaMissing}} {
		keys := make([]string, 0, len(group.counts))
		for key, n := range group.counts {
			if n > 0 {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %d\n", group.name, key, group.counts[key])
		}
	}
	if summary.SourceID != 0 {
		// Cache publication must include committed messages after interruption.
		err = errors.Join(err, rebuildCacheAfterWrite(context.WithoutCancel(ctx), state.cfg.DatabaseDSN(), state))
	}
	if err != nil {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Import failed after any progress shown above. Re-run to continue through deduplication.")
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Import complete. Coverage is limited to supported records; omissions are shown above.")
	return nil
}

func init() {
	rootCmd.AddCommand(newImportSignalExportCmd())
}
