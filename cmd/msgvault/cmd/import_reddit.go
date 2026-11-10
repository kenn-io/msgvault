package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/reddit"
)

type redditCLIOptions struct {
	Me                string
	NoDefaultIdentity bool
}

func newImportRedditCmd() *cobra.Command {
	var opts redditCLIOptions
	cmd := &cobra.Command{
		Use: "import-reddit <export-dir>", Short: "Import a Reddit account-data export",
		Long: `Import Chat, legacy private messages, and your own posts and comments from
an extracted Reddit account-data export directory. The owner comes from
statistics.csv; use --me when it is absent. Conflicting accounts are refused.

Reads top-level CSVs only, including numbered split files. At most 10,000
entries and 256 MiB per CSV are accepted. Links are preserved; media are not
downloaded. Unknown dates and senders remain unknown. Coverage depends on
what Reddit supplied; complete lifetime history is not guaranteed.

Re-running adds missing records and retains archived originals. After an
interruption, re-run the directory to finish. No records are deleted.

Example:
  msgvault import-reddit --me fictional_owner ./reddit-export`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args)
			}
			return runImportReddit(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.Me, "me", "", "your Reddit handle (fallback when statistics.csv has no account name)")
	cmd.Flags().BoolVar(&opts.NoDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
}

func runImportReddit(cmd *cobra.Command, dir string, opts redditCLIOptions) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Re-run the export directory to finish.")
	defer stop()
	summary, importErr := reddit.Import(ctx, s, reddit.ImportOptions{RootDir: dir, Me: opts.Me, Logger: state.logger})
	out := cmd.OutOrStdout()
	for _, f := range summary.Files {
		_, _ = fmt.Fprintf(out, "%s: %d rows, %d added, %d already archived (original retained), %d malformed, %d missing IDs; %d unknown dates, %d unknown senders, %d link/media rows not downloaded\n", f.Filename, f.Rows, f.Added, f.Unchanged, f.Malformed, f.MissingIDs, f.UnknownDates, f.UnknownSenders, f.LinksNotDownloaded)
	}
	for _, name := range summary.Unsupported {
		_, _ = fmt.Fprintf(out, "Unsupported: %s\n", name)
	}
	_, _ = fmt.Fprintf(out, "Total: %d added, %d already archived; %d malformed, %d missing IDs\n", summary.Added, summary.Unchanged, summary.Malformed, summary.MissingIDs)
	if summary.SourceID != 0 {
		// Refresh identities and cache for committed rows after an interrupted import.
		finalize := context.WithoutCancel(ctx)
		err = errors.Join(runExportPostImportMigrations(finalize, out, s, summary.SourceID, opts.NoDefaultIdentity, state), rebuildCacheAfterWrite(finalize, state.cfg.DatabaseDSN(), state))
	}
	return errors.Join(importErr, err)
}

func init() { rootCmd.AddCommand(newImportRedditCmd()) }
