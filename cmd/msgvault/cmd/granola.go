package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
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
	rebuildGranolaCacheAfterWrite         = rebuildCacheAfterWrite
	rebuildGranolaCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

const granolaConfigHint = `Add to your config.toml:

  [[granola]]
  identifier = "you@example.com"   # label for this account
  account_email = "you@example.com" # primary identity for organizer attribution
  api_key = "grn_..."              # from the desktop app's settings (Business plan)
  enabled = true
  # schedule = "0 */6 * * *"       # optional daemon schedule`

// resolveGranolaSource picks the [[granola]] entry for an optional CLI
// argument: an explicit identifier must match a configured entry; with no
// argument there must be exactly one entry.
func resolveGranolaSource(args []string) (*config.GranolaSource, error) {
	if len(cfg.Granola) == 0 {
		return nil, errors.New("no [[granola]] sources configured\n\n" + granolaConfigHint)
	}
	if len(args) > 0 {
		src := cfg.GetGranolaSource(args[0])
		if src == nil {
			var ids []string
			for _, s := range cfg.Granola {
				ids = append(ids, s.Identifier)
			}
			return nil, fmt.Errorf("no [[granola]] entry with identifier %q (configured: %s)", args[0], strings.Join(ids, ", "))
		}
		return src, nil
	}
	if len(cfg.Granola) > 1 {
		return nil, errors.New("multiple [[granola]] sources configured; pass an identifier")
	}
	src := cfg.Granola[0]
	return &src, nil
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
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}

		src, err := resolveGranolaSource(args)
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

		s, cleanup, err := openWritableStoreAndInitForIngest()
		if err != nil {
			return err
		}
		defer cleanup()

		if _, err := registerMeetingSource(
			cmd.OutOrStdout(), s, sourceTypeGranola, src.Identifier, accountEmail,
		); err != nil {
			return err
		}
		if err := runPostSourceCreateMigrations(s); err != nil {
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
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}

		var sources []config.GranolaSource
		if len(args) > 0 || len(cfg.Granola) == 1 {
			src, err := resolveGranolaSource(args)
			if err != nil {
				return err
			}
			sources = []config.GranolaSource{*src}
		} else {
			sources = cfg.Granola
		}
		if len(sources) == 0 {
			return errors.New("no [[granola]] sources configured\n\n" + granolaConfigHint)
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

		s, cleanup, err := openWritableStoreAndInitForIngest()
		if err != nil {
			return err
		}
		defer cleanup()
		dbPath := cfg.DatabaseDSN()

		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigChan)
		go func() {
			select {
			case <-sigChan:
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "\nInterrupted. Finishing current note...")
				cancel()
			case <-ctx.Done():
			}
		}()

		pendingCacheWrites := &granola.ImportSummary{}
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
				pendingCacheWrites.NotesAdded += sum.NotesAdded
				pendingCacheWrites.NotesUpdated += sum.NotesUpdated
			}
			if ctx.Err() != nil {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run sync-granola to resume.")
				return finishGranolaImport(src.Identifier, pendingCacheWrites, ctx.Err(), func() {
					rebuildGranolaCacheAfterWrite(dbPath)
				})
			}
			if finishErr := finishGranolaImport(src.Identifier, pendingCacheWrites, err, func() {
				rebuildGranolaCacheAfterWrite(dbPath)
			}); finishErr != nil {
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

		rebuildGranolaCacheAfterWrite(dbPath)
		return nil
	},
}

func finishGranolaImport(
	identifier string,
	sum *granola.ImportSummary,
	importErr error,
	refreshCache func(),
) error {
	if importErr == nil {
		return nil
	}
	if sum != nil && sum.NotesAdded+sum.NotesUpdated > 0 && refreshCache != nil {
		refreshCache()
	}
	return fmt.Errorf("granola sync %s failed: %w", identifier, importErr)
}

// runConfiguredGranolaSync is the daemon-scheduler entry point for one
// [[granola]] source.
func runConfiguredGranolaSync(ctx context.Context, st *store.Store, src config.GranolaSource) error {
	refreshCtx := context.WithoutCancel(ctx)
	// Generic scheduler jobs and mutating daemon requests share the operation
	// gate, so a registered source cannot be removed between this precheck and
	// the importer's existing-source GetOrCreateSource call.
	registered, err := st.ListSources(granola.SourceType)
	if err != nil {
		return fmt.Errorf("list registered Granola sources: %w", err)
	}
	found := false
	for _, candidate := range registered {
		if candidate.Identifier == src.Identifier {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("granola source %q is not registered; run msgvault add-granola %s",
			src.Identifier, src.Identifier)
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
	if err := finishGranolaImport(src.Identifier, sum, err, func() {
		rebuildGranolaCacheAfterScheduledSync(refreshCtx, "granola:"+src.Identifier)
	}); err != nil {
		return err
	}
	rebuildGranolaCacheAfterScheduledSync(refreshCtx, "granola:"+src.Identifier)
	return nil
}

func init() {
	syncGranolaCmd.Flags().IntVar(&syncGranolaLimit, "limit", 0, "max notes per run (0 = no limit)")
	syncGranolaCmd.Flags().StringVar(&syncGranolaAfter, "after", "", "full-sync only notes created after this date (YYYY-MM-DD; implies --full)")
	syncGranolaCmd.Flags().BoolVar(&syncGranolaFull, "full", false, "ignore stored cursor and re-fetch every note (repairs existing rows in place)")
	rootCmd.AddCommand(addGranolaCmd)
	rootCmd.AddCommand(syncGranolaCmd)
}
