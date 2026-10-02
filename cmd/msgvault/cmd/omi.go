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
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/omi"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

var (
	syncOmiLimit int
	syncOmiAfter string
	syncOmiFull  bool
)

var (
	newOmiClient                      = omi.NewClient
	rebuildOmiCacheAfterWrite         = rebuildCacheAfterManualSync
	rebuildOmiCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

const omiConfigHint = `Add to your config.toml:

  [[omi]]
  identifier = "you@example.com"   # label for this account
  account_email = "you@example.com" # primary archive identity
  api_key = "omi_dev_..."              # Developer API key with conversations:read
  # base_url = "http://localhost:8000" # self-hosted backend root
  enabled = true
  # schedule = "0 */6 * * *"       # optional daemon schedule`

// resolveOmiSource picks the [[omi]] entry for an optional CLI
// argument: an explicit identifier must match a configured entry; with no
// argument there must be exactly one entry.
func resolveOmiSource(args []string, cfg *config.Config) (*config.OmiSource, error) {
	if cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	if len(cfg.Omi) == 0 {
		return nil, errors.New("no [[omi]] sources configured\n\n" + omiConfigHint)
	}
	if len(args) > 0 {
		src := cfg.GetOmiSource(args[0])
		if src == nil {
			var ids []string
			for _, s := range cfg.Omi {
				ids = append(ids, s.Identifier)
			}
			return nil, fmt.Errorf("no [[omi]] entry with identifier %q (configured: %s)", args[0], strings.Join(ids, ", "))
		}
		return src, nil
	}
	if len(cfg.Omi) > 1 {
		return nil, errors.New("multiple [[omi]] sources configured; pass an identifier")
	}
	src := cfg.Omi[0]
	return &src, nil
}

var addOmiCmd = &cobra.Command{
	Use:   "add-omi [identifier]",
	Short: "Register a Omi account and validate its API key",
	Long: `Register a configured Omi account as a msgvault source.

Reads the API key from the matching [[omi]] entry in config.toml and
validates conversation and transcript access with a live API call. Create a
Developer API key with conversations:read in Omi Settings > Developer.
Set base_url to your backend root for a self-hosted instance.

Examples:
  msgvault add-omi
  msgvault add-omi you@example.com`,
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

		src, err := resolveOmiSource(args, cfg)
		if err != nil {
			return err
		}
		accountEmail, err := src.EffectiveAccountEmail()
		if err != nil {
			return err
		}
		if src.APIKey == "" {
			return fmt.Errorf("[[omi]] entry %q has no api_key\n\n%s", src.Identifier, omiConfigHint)
		}

		// Probe conversation access with transcript inclusion.
		client := newOmiClient(src.BaseURL, src.APIKey)
		if _, err := client.ListConversations(cmd.Context(), omi.ListParams{Limit: 1}); err != nil {
			return fmt.Errorf("validate Omi API key: %w", err)
		}

		s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()

		if _, err := registerMeetingSource(
			cmd.OutOrStdout(), s, sourceTypeOmi, src.Identifier, accountEmail,
		); err != nil {
			return err
		}
		if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}

		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nOmi account registered successfully!\n")
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Identifier: %s\n\n", src.Identifier)
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "You can now run:")
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  msgvault sync-omi %s\n", src.Identifier)
		return nil
	},
}

var syncOmiCmd = &cobra.Command{
	Use:   "sync-omi [identifier]",
	Short: "Sync Omi meeting conversations and transcripts",
	Long: `Sync meeting conversations and transcripts from Omi.

Each run rescans accessible completed conversations and skips unchanged archive
writes so edits to older conversations remain discoverable. With no identifier, every configured [[omi]] source is synced.

Use --full to rewrite derived projections for every conversation; --after
bounds a full sync to conversations created after the given date. Re-fetched conversations
are upserted in place, so --full repairs existing rows without duplicates.

Examples:
  msgvault sync-omi
  msgvault sync-omi you@example.com --limit 5
  msgvault sync-omi --full --after 2024-01-01`,
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

		var sources []config.OmiSource
		if len(args) > 0 || len(cfg.Omi) == 1 {
			src, err := resolveOmiSource(args, cfg)
			if err != nil {
				return err
			}
			sources = []config.OmiSource{*src}
		} else {
			sources = cfg.Omi
		}
		if len(sources) == 0 {
			return errors.New("no [[omi]] sources configured\n\n" + omiConfigHint)
		}

		if syncOmiLimit < 0 {
			return usageErr(cmd, errors.New("--limit cannot be negative"))
		}
		var after time.Time
		if syncOmiAfter != "" {
			t, err := time.Parse("2006-01-02", syncOmiAfter)
			if err != nil {
				return usageErr(cmd, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD): %w", syncOmiAfter, err))
			}
			after = t.UTC()
		}
		type validatedOmiSource struct {
			source       config.OmiSource
			accountEmail string
		}
		validatedSources := make([]validatedOmiSource, 0, len(sources))
		for _, src := range sources {
			accountEmail, err := src.EffectiveAccountEmail()
			if err != nil {
				return err
			}
			if src.APIKey == "" {
				return fmt.Errorf("[[omi]] entry %q has no api_key", src.Identifier)
			}
			validatedSources = append(validatedSources, validatedOmiSource{
				source: src, accountEmail: accountEmail,
			})
		}

		s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
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
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "\nInterrupted. Finishing current conversation...")
				cancel()
			case <-ctx.Done():
			}
		}()

		pendingCacheWrites := &omi.ImportSummary{}
		for _, validated := range validatedSources {
			src := validated.source
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Omi for %s\n\n", src.Identifier)

			imp := omi.NewImporter(s, newOmiClient(src.BaseURL, src.APIKey))
			sum, err := imp.Import(ctx, omi.ImportOptions{
				Identifier:   src.Identifier,
				AccountEmail: validated.accountEmail,
				Full:         syncOmiFull || !after.IsZero(),
				Limit:        syncOmiLimit,
				CreatedAfter: after,
				Progress:     func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) },
			})
			if sum != nil {
				pendingCacheWrites.MeetingsAdded += sum.MeetingsAdded
				pendingCacheWrites.MeetingsUpdated += sum.MeetingsUpdated
			}
			if ctx.Err() != nil {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run sync-omi to resume.")
				return finishOmiImport(src.Identifier, pendingCacheWrites, ctx.Err(), func() error {
					return rebuildOmiCacheAfterWrite(dbPath, state)
				})
			}
			if finishErr := finishOmiImport(src.Identifier, pendingCacheWrites, err, func() error {
				return rebuildOmiCacheAfterWrite(dbPath, state)
			}); finishErr != nil {
				return finishErr
			}

			_, _ = fmt.Fprintln(cmd.OutOrStdout())
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Omi sync complete!")
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Duration:        %s\n", sum.Duration.Round(time.Second))
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Meetings processed: %d\n", sum.MeetingsProcessed)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Meetings added:     %d\n", sum.MeetingsAdded)
			if sum.Errors > 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Errors:          %d\n", sum.Errors)
			}
		}

		return rebuildOmiCacheAfterWrite(dbPath, state)
	},
}

func finishOmiImport(
	identifier string,
	sum *omi.ImportSummary,
	importErr error,
	refreshCache func() error,
) error {
	if importErr == nil {
		return nil
	}
	var refreshErr error
	if sum != nil && sum.MeetingsAdded+sum.MeetingsUpdated > 0 && refreshCache != nil {
		refreshErr = refreshCache()
	}
	return errors.Join(fmt.Errorf("omi sync %s failed: %w", identifier, importErr), refreshErr)
}

// runConfiguredOmiSync is the daemon-scheduler entry point for one
// [[omi]] source.
func runConfiguredOmiSync(ctx context.Context, st *store.Store, src config.OmiSource) error {
	refreshCtx := context.WithoutCancel(ctx)
	// Generic scheduler jobs and mutating daemon requests share the operation
	// gate, so a registered source cannot be removed between this precheck and
	// the importer's registered-source lookup.
	registered, err := st.ListSources(omi.SourceType)
	if err != nil {
		return fmt.Errorf("list registered Omi sources: %w", err)
	}
	found := false
	for _, candidate := range registered {
		if candidate.Identifier == src.Identifier {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("omi source %q is not registered; run msgvault add-omi %s",
			src.Identifier, src.Identifier)
	}
	if src.APIKey == "" {
		return fmt.Errorf("omi source %q has no api_key", src.Identifier)
	}
	accountEmail, err := src.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	imp := omi.NewImporter(st, newOmiClient(src.BaseURL, src.APIKey))
	sum, err := imp.Import(ctx, omi.ImportOptions{
		Identifier:   src.Identifier,
		AccountEmail: accountEmail,
	})
	if err := finishOmiImport(src.Identifier, sum, err, func() error {
		return rebuildOmiCacheAfterScheduledSync(refreshCtx, "omi:"+src.Identifier)
	}); err != nil {
		return err
	}
	return rebuildOmiCacheAfterScheduledSync(refreshCtx, "omi:"+src.Identifier)
}

func init() {
	syncOmiCmd.Flags().IntVar(&syncOmiLimit, "limit", 0, "max conversations per run (0 = no limit)")
	syncOmiCmd.Flags().StringVar(&syncOmiAfter, "after", "", "full-sync only conversations created after this date (YYYY-MM-DD; implies --full)")
	syncOmiCmd.Flags().BoolVar(&syncOmiFull, "full", false, "rewrite every conversation projection (repairs existing rows in place)")
	rootCmd.AddCommand(addOmiCmd)
	rootCmd.AddCommand(addManualSyncCacheFlags(syncOmiCmd))
}

// registerScheduledOmiJob uses the same source-to-job mapping as API status.
func registerScheduledOmiJob(sched *scheduler.Scheduler, state *invocation, st *store.Store, source config.OmiSource) error {
	jobName, ok := api.SchedulerJobNameForSource(omi.SourceType, source.Identifier)
	if !ok {
		return fmt.Errorf("no scheduler job mapping for omi source %q", source.Identifier)
	}
	return sched.AddJob(scheduler.Job{
		Name:     jobName,
		Schedule: source.Schedule,
		Run: invocationBoundJobRun(state, func(ctx context.Context) error {
			return runConfiguredOmiSync(ctx, st, source)
		}),
	})
}
