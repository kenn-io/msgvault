package cmd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/teams"
)

var (
	syncTeamsNoChannels bool
	syncTeamsLimit      int
	syncTeamsFull       bool
)

var syncTeamsCmd = &cobra.Command{
	Use:   "sync-teams <email>",
	Short: "Sync Microsoft Teams chats and channels (full or incremental)",
	Long: `Sync Microsoft Teams chats and channels for a configured account.

Full or incremental sync is auto-detected based on what has already been
imported. Re-run to resume after an interruption.

Use --full to ignore the stored cursor and re-fetch every message (e.g. to
backfill fields added by an importer upgrade). Re-fetched messages are
upserted in place, so this repairs existing rows without creating duplicates.

Examples:
  msgvault sync-teams user@company.com
  msgvault sync-teams user@company.com --no-channels
  msgvault sync-teams user@company.com --limit 100
  msgvault sync-teams user@company.com --full`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil || state.logger == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		logger := state.logger
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}

		email := args[0]

		s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()

		if err := requireMicrosoftOAuthConfig(cfg); err != nil {
			return err
		}
		client, err := newTeamsClient(cmd.Context(), cfg, logger, email)
		if err != nil {
			return fmt.Errorf("load Teams token: %w (run 'add-teams' first)", err)
		}

		ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving checkpoint...")
		defer stop()

		imp := teams.NewImporter(s, client)

		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Microsoft Teams for %s\n\n", email)

		opts := teams.ImportOptions{
			Email:           email,
			AttachmentsDir:  cfg.AttachmentsDir(),
			MediaPolicy:     cfg.Teams.MediaPolicy(email),
			IncludeChannels: !syncTeamsNoChannels,
			Limit:           syncTeamsLimit,
			Full:            syncTeamsFull,
			Progress:        func(s string) { fmt.Println(s) },
		}
		sum, err := imp.Import(ctx, opts)
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run sync-teams to resume.")
			return rebuildCacheAfterManualSync(dbPath, state)
		}
		if err != nil {
			return fmt.Errorf("teams sync failed: %w", err)
		}

		writeTeamsSyncSummary(cmd.OutOrStdout(), sum)

		return rebuildCacheAfterManualSync(dbPath, state)
	},
}

func writeTeamsSyncSummary(out io.Writer, sum *teams.ImportSummary) {
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Teams sync complete!")
	_, _ = fmt.Fprintf(out, "  Duration:        %s\n", sum.Duration.Round(time.Second))
	_, _ = fmt.Fprintf(out, "  Chats:           %d\n", sum.ChatsProcessed)
	_, _ = fmt.Fprintf(out, "  Channels:        %d\n", sum.ChannelsProcessed)
	_, _ = fmt.Fprintf(out, "  Messages added:  %d\n", sum.MessagesAdded)
	_, _ = fmt.Fprintf(out, "  Reactions:       %d\n", sum.ReactionsAdded)
	_, _ = fmt.Fprintf(out, "  Attachments:     %d\n", sum.AttachmentsFound)
	_, _ = fmt.Fprintf(out, "  Inline images:   %d\n", sum.InlineImagesCopied)
	_, _ = fmt.Fprintf(out, "  Inline skipped by policy: %d\n", sum.InlineImagesSkipped)
	if sum.Errors > 0 {
		_, _ = fmt.Fprintf(out, "  Errors:          %d\n", sum.Errors)
	}
}

func init() {
	syncTeamsCmd.Flags().BoolVar(&syncTeamsNoChannels, "no-channels", false, "sync chats only (skip team channels)")
	syncTeamsCmd.Flags().IntVar(&syncTeamsLimit, "limit", 0, "max messages per conversation (0 = no limit)")
	syncTeamsCmd.Flags().BoolVar(&syncTeamsFull, "full", false, "ignore stored cursor and re-fetch every message (repairs/backfills existing rows in place)")
	rootCmd.AddCommand(addManualSyncCacheFlags(syncTeamsCmd))
}
