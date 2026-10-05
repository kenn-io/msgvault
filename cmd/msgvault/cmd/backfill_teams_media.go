package cmd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
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
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
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

		ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Stopping...")
		defer stop()

		imp := teams.NewImporter(s, client)

		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Backfilling Teams inline media for %s\n\n", email)

		sum, err := imp.BackfillInlineMedia(ctx, teams.ImportOptions{
			Email:          email,
			AttachmentsDir: cfg.AttachmentsDir(),
			MediaPolicy:    cfg.Teams.MediaPolicy(email),
			OnlyIncomplete: backfillTeamsMediaOnlyIncomplete,
			Progress:       func(s string) { fmt.Println(s) },
		})
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run backfill-teams-media to resume (idempotent).")
			return rebuildCacheAfterWrite(dbPath, state)
		}
		if err != nil {
			return fmt.Errorf("teams inline-media backfill failed: %w", err)
		}

		writeTeamsMediaBackfillSummary(cmd.OutOrStdout(), sum)

		return rebuildCacheAfterWrite(dbPath, state)
	},
}

func writeTeamsMediaBackfillSummary(out io.Writer, sum *teams.ImportSummary) {
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Teams inline-media backfill complete!")
	_, _ = fmt.Fprintf(out, "  Duration:             %s\n", sum.Duration.Round(time.Second))
	_, _ = fmt.Fprintf(out, "  Messages processed:   %d\n", sum.MessagesProcessed)
	_, _ = fmt.Fprintf(out, "  Inline images copied: %d\n", sum.InlineImagesCopied)
	_, _ = fmt.Fprintf(out, "  Skipped by policy:    %d\n", sum.InlineImagesSkipped)
	_, _ = fmt.Fprintf(out, "  Errors:               %d\n", sum.Errors)
}

func init() {
	backfillTeamsMediaCmd.Flags().BoolVar(&backfillTeamsMediaOnlyIncomplete, "only-incomplete", false,
		"retry only messages whose inline media is still missing (e.g. after transient failures)")
	rootCmd.AddCommand(backfillTeamsMediaCmd)
}
