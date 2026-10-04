package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/store"
)

var (
	syncGranolaLimit int
	syncGranolaAfter string
	syncGranolaFull  bool
)

var (
	newGranolaClient                      = granola.NewClient
	rebuildGranolaCacheAfterWrite         = rebuildCacheAfterManualSync
	rebuildGranolaCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

const granolaConfigHint = `Add to your config.toml:

  [[granola]]
  identifier = "you@example.com"   # label for this account
  account_email = "you@example.com" # primary identity for organizer attribution
  api_key = "grn_..."              # from the desktop app's settings (Business plan)
  enabled = true
  # schedule = "0 */6 * * *"       # optional daemon schedule`

func granolaSources(cfg *config.Config) meetingSources[config.GranolaSource] {
	sources := meetingSources[config.GranolaSource]{table: "granola", hint: granolaConfigHint}
	if cfg != nil {
		sources.configured, sources.lookup = cfg.Granola, cfg.GetGranolaSource
		sources.identifier = func(s config.GranolaSource) string { return s.Identifier }
	}
	return sources
}

var addGranolaCmd = &cobra.Command{
	Use:   "add-granola [identifier]",
	Short: "Register a Granola account and validate its API key",
	Long: `Register a configured Granola account as a msgvault source.

Reads the API key from the matching [[granola]] entry in config.toml and
validates it with a live API call. Granola API keys are created in the
desktop app's settings and require a Business plan.

Examples:
  msgvault add-granola
  msgvault add-granola you@example.com`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}

		src, err := granolaSources(cfg).one(args)
		if err != nil {
			return err
		}
		accountEmail, err := src.EffectiveAccountEmail()
		if err != nil {
			return err
		}
		if src.APIKey == "" {
			return fmt.Errorf("[[granola]] entry %q has no api_key\n\n%s", src.Identifier, granolaConfigHint)
		}

		// Live probe: one note is enough to prove the key works.
		client := granola.NewClient(granola.DefaultBaseURL, src.APIKey)
		if _, err := client.ListNotes(cmd.Context(), granola.ListNotesParams{PageSize: 1}); err != nil {
			return fmt.Errorf("validate Granola API key: %w", err)
		}

		s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()

		if _, err := registerMeetingSource(
			cmd.OutOrStdout(), s, sourceTypeGranola, src.Identifier, accountEmail,
		); err != nil {
			return err
		}
		if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}

		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nGranola account registered successfully!\n")
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Identifier: %s\n\n", src.Identifier)
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "You can now run:")
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  msgvault sync-granola %s\n", src.Identifier)
		return nil
	},
}

var syncGranolaCmd = &cobra.Command{
	Use:   "sync-granola [identifier]",
	Short: "Sync Granola meeting notes and transcripts",
	Long: `Sync meeting notes and transcripts from Granola.

Incremental by default: only notes updated since the last successful run are
fetched. With no identifier, every configured [[granola]] source is synced.

Use --full to ignore the stored cursor and re-fetch everything; --after
bounds a full sync to notes created after the given date. Re-fetched notes
are upserted in place, so --full repairs existing rows without duplicates.

Examples:
  msgvault sync-granola
  msgvault sync-granola you@example.com --limit 5
  msgvault sync-granola --full --after 2024-01-01`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		cfg := state.cfg
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}

		sources, err := granolaSources(cfg).selected(args)
		if err != nil {
			return err
		}

		var after time.Time
		if syncGranolaAfter != "" {
			t, err := time.Parse("2006-01-02", syncGranolaAfter)
			if err != nil {
				return usageErr(cmd, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD): %w", syncGranolaAfter, err))
			}
			after = t.UTC()
		}
		type validatedGranolaSource struct {
			source       config.GranolaSource
			accountEmail string
		}
		validatedSources := make([]validatedGranolaSource, 0, len(sources))
		for _, src := range sources {
			accountEmail, err := src.EffectiveAccountEmail()
			if err != nil {
				return err
			}
			if src.APIKey == "" {
				return fmt.Errorf("[[granola]] entry %q has no api_key", src.Identifier)
			}
			validatedSources = append(validatedSources, validatedGranolaSource{
				source: src, accountEmail: accountEmail,
			})
		}

		s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()

		ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Finishing current note...")
		defer stop()

		var pendingWrites int64
		refresh := func() error { return rebuildGranolaCacheAfterWrite(dbPath, state) }
		for _, validated := range validatedSources {
			src := validated.source
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Granola for %s\n\n", src.Identifier)

			imp := granola.NewImporter(s, newGranolaClient(granola.DefaultBaseURL, src.APIKey))
			sum, err := imp.Import(ctx, granola.ImportOptions{
				Identifier:   src.Identifier,
				AccountEmail: validated.accountEmail,
				Full:         syncGranolaFull || !after.IsZero(),
				Limit:        syncGranolaLimit,
				CreatedAfter: after,
				Progress:     func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) },
			})
			if sum != nil {
				pendingWrites += sum.NotesAdded + sum.NotesUpdated
			}
			if ctx.Err() != nil {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run sync-granola to resume.")
				return finishMeetingImport("granola", src.Identifier, pendingWrites, ctx.Err(), nil, refresh)
			}
			if finishErr := finishMeetingImport("granola", src.Identifier, pendingWrites, err, nil, refresh); finishErr != nil {
				return finishErr
			}

			_, _ = fmt.Fprintln(cmd.OutOrStdout())
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Granola sync complete!")
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Duration:        %s\n", sum.Duration.Round(time.Second))
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Notes processed: %d\n", sum.NotesProcessed)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Notes added:     %d\n", sum.NotesAdded)
			if sum.Errors > 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Errors:          %d\n", sum.Errors)
			}
		}

		return rebuildGranolaCacheAfterWrite(dbPath, state)
	},
}

// runConfiguredGranolaSync is the daemon-scheduler entry point for one
// [[granola]] source.
func runConfiguredGranolaSync(ctx context.Context, st *store.Store, src config.GranolaSource) error {
	// Generic scheduler jobs and mutating daemon requests share the operation
	// gate, so a registered source cannot be removed between this precheck and
	// the importer's existing-source GetOrCreateSource call.
	if err := requireRegisteredMeetingSource(st, granola.SourceType, src.Identifier, fmt.Errorf(
		"granola source %q is not registered; run msgvault add-granola %s", src.Identifier, src.Identifier)); err != nil {
		return err
	}
	if src.APIKey == "" {
		return fmt.Errorf("granola source %q has no api_key", src.Identifier)
	}
	accountEmail, err := src.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	imp := granola.NewImporter(st, newGranolaClient(granola.DefaultBaseURL, src.APIKey))
	sum, err := imp.Import(ctx, granola.ImportOptions{
		Identifier:   src.Identifier,
		AccountEmail: accountEmail,
	})
	var writes int64
	if sum != nil {
		writes = sum.NotesAdded + sum.NotesUpdated
	}
	return finishScheduledMeetingImport(ctx, "granola", src.Identifier, "granola:"+src.Identifier, writes, err, nil,
		rebuildGranolaCacheAfterScheduledSync)
}

func init() {
	syncGranolaCmd.Flags().IntVar(&syncGranolaLimit, "limit", 0, "max notes per run (0 = no limit)")
	syncGranolaCmd.Flags().StringVar(&syncGranolaAfter, "after", "", "full-sync only notes created after this date (YYYY-MM-DD; implies --full)")
	syncGranolaCmd.Flags().BoolVar(&syncGranolaFull, "full", false, "ignore stored cursor and re-fetch every note (repairs existing rows in place)")
	rootCmd.AddCommand(addGranolaCmd)
	rootCmd.AddCommand(addManualSyncCacheFlags(syncGranolaCmd))
}
