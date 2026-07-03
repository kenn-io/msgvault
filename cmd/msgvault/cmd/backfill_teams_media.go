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

var backfillTeamsMediaOnlyIncomplete bool

var backfillTeamsMediaCmd = &cobra.Command{
	Use:   "backfill-teams-media <email>",
	Short: "Re-fetch Teams inline media (hostedContents) for already-imported messages",
	Long: `Re-fetch Microsoft Teams inline media (hostedContents) for messages that
were already imported but whose inline images were never downloaded.

This targets ONLY messages whose stored HTML body contains a hostedContents
URL, instead of re-walking every message. It is idempotent: content-addressed
storage dedupes, so it is safe to re-run.

Use --only-incomplete to retry just the messages whose inline media is still
missing (e.g. after transient fetch failures), instead of re-fetching all.

Examples:
  msgvault backfill-teams-media user@company.com
  msgvault backfill-teams-media user@company.com --only-incomplete`,
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
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "\nInterrupted. Stopping...")
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

		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Backfilling Teams inline media for %s\n\n", email)

		sum, err := imp.BackfillInlineMedia(ctx, teams.ImportOptions{
			Email:          email,
			AttachmentsDir: cfg.AttachmentsDir(),
			OnlyIncomplete: backfillTeamsMediaOnlyIncomplete,
			Progress:       func(s string) { fmt.Println(s) },
		})
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run backfill-teams-media to resume (idempotent).")
			rebuildCacheAfterWrite(dbPath)
			return nil
		}
		if err != nil {
			return fmt.Errorf("teams inline-media backfill failed: %w", err)
		}

		_, _ = fmt.Fprintln(cmd.OutOrStdout())
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Teams inline-media backfill complete!")
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Duration:            %s\n", sum.Duration.Round(time.Second))
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages processed:  %d\n", sum.MessagesProcessed)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Inline images copied:%d\n", sum.InlineImagesCopied)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Errors:              %d\n", sum.Errors)

		rebuildCacheAfterWrite(dbPath)
		return nil
	},
}

func init() {
	backfillTeamsMediaCmd.Flags().BoolVar(&backfillTeamsMediaOnlyIncomplete, "only-incomplete", false,
		"retry only messages whose inline media is still missing (e.g. after transient failures)")
	rootCmd.AddCommand(backfillTeamsMediaCmd)
}
