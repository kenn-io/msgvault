package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/linkedin"
)

func newImportLinkedInCmd() *cobra.Command {
	var opts linkedin.Options
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "linkedin <identifier> <archive>",
		Short: "Import a LinkedIn member data export",
		Long: `Import messages, your own comments and posts from a LinkedIn data export ZIP
or extracted folder. Connections.csv enriches people without creating messages.
Use the same identifier for newer exports. --me is your LinkedIn profile URL.
Attachment URLs are archived as links; no network requests are made.`,
	}
	report := func(cmd *cobra.Command, err error) error {
		if err == nil {
			return nil
		}
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		// Proxied failures have already been rendered by the daemon subprocess.
		if _, ok := errors.AsType[proxiedChildError](err); ok {
			return err
		}
		hint := "use 'msgvault import linkedin <identifier> <archive> --me <profile-url>'; repeat an interrupted import to continue"
		if jsonOutput {
			_ = printJSONTo(cmd.ErrOrStderr(), map[string]string{"error": err.Error(), "hint": hint})
		} else {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "error: %s\nhint: %s\n", err, hint)
		}
		return err
	}
	command.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) != 2 {
			return report(cmd, errors.New("expected an identifier and an archive ZIP or folder"))
		}
		return nil
	}
	command.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error { return report(cmd, err) })
	command.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		for parent := cmd.Parent(); parent != nil; parent = parent.Parent() {
			if parent.PersistentPreRunE != nil {
				return report(cmd, parent.PersistentPreRunE(cmd, args))
			}
		}
		return nil
	}
	command.PreRunE = func(cmd *cobra.Command, args []string) error {
		if opts.Me == "" {
			return report(cmd, errors.New("--me is required"))
		}
		if _, err := linkedin.ProfileURL(opts.Me); err != nil {
			return report(cmd, err)
		}
		return nil
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return report(cmd, runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args, nil))
		}
		opts.Identifier = args[0]
		return report(cmd, runImportLinkedIn(cmd, args[1], opts, jsonOutput))
	}
	command.Flags().StringVar(&opts.Me, "me", "", "your LinkedIn member profile URL (required)")
	command.Flags().StringVar(&opts.DisplayName, "display-name", "", "your display name for posts and comments")
	command.Flags().BoolVar(&opts.IncludeDrafts, "include-drafts", false, "include unsent message drafts")
	command.Flags().BoolVar(&jsonOutput, "json", false, "output JSON; errors go to stderr")
	return command
}

func runImportLinkedIn(cmd *cobra.Command, sourcePath string, opts linkedin.Options, jsonOutput bool) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Repeat the import to continue.")
	defer stop()
	summary, importErr := linkedin.Import(ctx, st, sourcePath, opts)
	var migrationErr error
	if summary != nil {
		migrationErr = runPostSourceCreateMigrationsForInvocation(st, state)
	}
	if err := errors.Join(importErr, migrationErr, rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state)); err != nil {
		return err
	}
	if jsonOutput {
		return printJSONTo(cmd.OutOrStdout(), summary)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Import complete!\n  Messages: %d added, %d updated\n  Drafts skipped: %d\n  Connections: %d\n", summary.MessagesAdded, summary.MessagesUpdated, summary.DraftsSkipped, summary.ConnectionsProcessed)
	if err != nil {
		return fmt.Errorf("write LinkedIn import summary: %w", err)
	}
	return nil
}

func init() {
	importCmd.Hidden = false
	importCmd.Deprecated = ""
	importCmd.Short = "Import a local message export"
	importCmd.AddCommand(newImportLinkedInCmd())
}
