package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/microsoft"
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
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}

		email := args[0]

		s, cleanup, err := openWritableStoreAndInitForIngest()
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()

		if cfg.Microsoft.ClientID == "" {
			return errors.New("microsoft OAuth not configured\n\n" +
				"Add to your config.toml:\n\n" +
				"  [microsoft]\n" +
				"  client_id = \"your-azure-app-client-id\"\n\n" +
				"See docs for Azure AD app registration setup")
		}

		mgr := microsoft.NewGraphManager(
			cfg.Microsoft.ClientID,
			cfg.Microsoft.EffectiveTenantID(),
			cfg.TokensDir(),
			logger,
		)
		tokenFn, err := mgr.TokenSource(cmd.Context(), email)
		if err != nil {
			return fmt.Errorf("load Teams token: %w (run 'add-teams' first)", err)
		}

		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigChan)
		go func() {
			select {
			case <-sigChan:
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "\nInterrupted. Saving checkpoint...")
				cancel()
			case <-ctx.Done():
			}
		}()

		qps := float64(cfg.Sync.RateLimitQPS)
		if qps <= 0 {
			qps = 5
		}
		client := teams.NewClient("https://graph.microsoft.com/v1.0", teams.TokenFunc(tokenFn), qps)
		imp := teams.NewImporter(s, client)

		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Microsoft Teams for %s\n\n", email)

		opts := teams.ImportOptions{
			Email:           email,
			AttachmentsDir:  cfg.AttachmentsDir(),
			IncludeChannels: !syncTeamsNoChannels,
			Limit:           syncTeamsLimit,
			Full:            syncTeamsFull,
			Progress:        func(s string) { fmt.Println(s) },
		}
		sum, err := imp.Import(ctx, opts)
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run sync-teams to resume.")
			rebuildCacheAfterWrite(dbPath)
			return nil
		}
		if err != nil {
			return fmt.Errorf("teams sync failed: %w", err)
		}

		_, _ = fmt.Fprintln(cmd.OutOrStdout())
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Teams sync complete!")
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Duration:        %s\n", sum.Duration.Round(time.Second))
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Chats:           %d\n", sum.ChatsProcessed)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Channels:        %d\n", sum.ChannelsProcessed)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages added:  %d\n", sum.MessagesAdded)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Reactions:       %d\n", sum.ReactionsAdded)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Attachments:     %d\n", sum.AttachmentsFound)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Inline images:   %d\n", sum.InlineImagesCopied)
		if sum.Errors > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Errors:          %d\n", sum.Errors)
		}

		rebuildCacheAfterWrite(dbPath)
		return nil
	},
}

func init() {
	syncTeamsCmd.Flags().BoolVar(&syncTeamsNoChannels, "no-channels", false, "sync chats only (skip team channels)")
	syncTeamsCmd.Flags().IntVar(&syncTeamsLimit, "limit", 0, "max messages per conversation (0 = no limit)")
	syncTeamsCmd.Flags().BoolVar(&syncTeamsFull, "full", false, "ignore stored cursor and re-fetch every message (repairs/backfills existing rows in place)")
	rootCmd.AddCommand(syncTeamsCmd)
}
