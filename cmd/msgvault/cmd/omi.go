package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
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
	newOmiClient                      = omi.NewPacedClient
	rebuildOmiCacheAfterWrite         = rebuildCacheAfterManualSync
	rebuildOmiCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

// omiPaceDir holds request pacing shared by the daemon and its subprocesses.
func omiPaceDir(cfg *config.Config) string {
	if cfg.Data.DataDir == "" {
		return ""
	}
	return filepath.Join(cfg.Data.DataDir, "omi")
}

const omiConfigHint = `Add to your config.toml:

  [[omi]]
  identifier = "you@example.com"   # label for this account
  account_email = "you@example.com" # primary archive identity
  api_key = "omi_dev_..."              # Developer API key with conversations:read
  # base_url = "http://localhost:8000" # self-hosted backend root
  enabled = true
  # schedule = "0 */6 * * *"       # optional daemon schedule`

const omiPassRuntime = 5 * time.Minute

func omiSources(cfg *config.Config) meetingSources[config.OmiSource] {
	m := meetingSources[config.OmiSource]{table: "omi", hint: omiConfigHint, identifier: func(s config.OmiSource) string { return s.Identifier }}
	if cfg != nil {
		m.configured, m.lookup = cfg.Omi, cfg.GetOmiSource
	}
	return m
}

func resolveOmiSource(args []string, cfg *config.Config) (*config.OmiSource, error) {
	return omiSources(cfg).one(args)
}

var addOmiCmd = &cobra.Command{
	Use:   "add-omi [identifier]",
	Short: "Register an Omi account and validate its API key",
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
		client := newOmiClient(src.BaseURL, src.APIKey, omiPaceDir(cfg))
		ctx, cancel := context.WithTimeout(cmd.Context(), omiPassRuntime)
		defer cancel()
		if _, err := client.ListConversations(ctx, omi.ListParams{Limit: 1}); err != nil {
			return fmt.Errorf("validate Omi API key: %w", err)
		}

		s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()

		if _, err := registerMeetingSource(
			cmd.OutOrStdout(), s, omi.SourceType, src.Identifier, accountEmail,
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

Each source gets a resumable pass of up to five minutes. Run the same
command again to continue its saved creation-date window. The first scan reads accessible
completed conversations. Later runs read
conversations created since the last complete sync, reaching back 48 hours to
pick up late processing and recent edits, and skip unchanged archive writes.
With no identifier, every configured [[omi]] source is synced.

Use --full to rescan history, picking up edits to older conversations and
rewriting derived projections; --after bounds a full sync to conversations
created after the given date. Re-fetched conversations are upserted in place,
so --full repairs existing rows without duplicates.

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

		sources, err := omiSources(cfg).selected(args)
		if err != nil {
			return err
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

		interruptCtx, stop := withInterruptCancel(cmd, "\nInterrupted. Finishing current conversation...")
		defer stop()

		var pendingWrites int64
		finish := func(identifier string, err error) error {
			return (meetingSyncRun{provider: "omi", identifier: identifier, writes: pendingWrites, err: err}).finish(func() error { return rebuildOmiCacheAfterWrite(dbPath, state) })
		}
		for _, validated := range validatedSources {
			src := validated.source
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Syncing Omi for %s\n\n", src.Identifier)

			ctx, cancel := context.WithTimeoutCause(interruptCtx, omiPassRuntime, jobctx.ErrRunBudgetExceeded)
			imp := omi.NewImporter(s, newOmiClient(src.BaseURL, src.APIKey, omiPaceDir(cfg)))
			sum, err := imp.Import(ctx, omi.ImportOptions{
				Identifier:   src.Identifier,
				AccountEmail: validated.accountEmail,
				Full:         syncOmiFull || !after.IsZero(),
				Limit:        syncOmiLimit,
				CreatedAfter: after,
				ExplicitScan: cmd.Flags().Changed("full") || cmd.Flags().Changed("limit") || cmd.Flags().Changed("after"),
				Progress:     func(line string) { _, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+line) },
			})
			cancel()
			if sum != nil {
				pendingWrites += sum.MeetingsAdded + sum.MeetingsUpdated
			}
			if err == nil && sum != nil && sum.PauseReason != nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Omi sync for %s paused. Run the same command again", src.Identifier)
				if sum != nil && sum.CheckpointSaved {
					_, _ = fmt.Fprint(cmd.OutOrStdout(), " to continue the saved scan")
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), ".")
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), sum.PauseReason)
				continue
			}
			if interruptCtx.Err() != nil {
				_, _ = fmt.Fprint(cmd.OutOrStdout(), "\nInterrupted.")
				if sum != nil && sum.CheckpointSaved {
					_, _ = fmt.Fprint(cmd.OutOrStdout(), " Run sync-omi again to resume.")
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout())
				if err == nil {
					err = interruptCtx.Err()
				}
				return finish(src.Identifier, err)
			}
			if finishErr := finish(src.Identifier, err); finishErr != nil {
				return finishErr
			}

			_, _ = fmt.Fprintln(cmd.OutOrStdout())
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Omi sync complete!")
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Duration:           %s\n", sum.Duration.Round(time.Second))
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Meetings processed: %d\n", sum.MeetingsProcessed)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Meetings added:     %d\n", sum.MeetingsAdded)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Meetings updated:   %d\n", sum.MeetingsUpdated)
			if sum.Errors > 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Errors:             %d\n", sum.Errors)
			}
		}

		return rebuildOmiCacheAfterWrite(dbPath, state)
	},
}

// runConfiguredOmiSync is the daemon-scheduler entry point for one
// [[omi]] source.
func runConfiguredOmiSync(ctx context.Context, st *store.Store, paceDir string, src config.OmiSource) error {
	if src.APIKey == "" {
		return fmt.Errorf("omi source %q has no api_key", src.Identifier)
	}
	accountEmail, err := src.EffectiveAccountEmail()
	if err != nil {
		return err
	}
	imp := omi.NewImporter(st, newOmiClient(src.BaseURL, src.APIKey, paceDir))
	sum, err := imp.Import(ctx, omi.ImportOptions{
		Identifier:   src.Identifier,
		AccountEmail: accountEmail,
	})
	var writes int64
	if sum != nil {
		writes = sum.MeetingsAdded + sum.MeetingsUpdated
	}
	err = (meetingSyncRun{provider: "omi", identifier: src.Identifier, writes: writes, err: err}).finishScheduled(ctx, "omi:"+src.Identifier, rebuildOmiCacheAfterScheduledSync)
	if err == nil && sum != nil && sum.PauseReason != nil {
		var cooldown *omi.CooldownError
		if errors.As(sum.PauseReason, &cooldown) || (!jobctx.HasProgress(ctx) && errors.Is(context.Cause(ctx), jobctx.ErrRunBudgetExceeded)) {
			return scheduler.ErrDeferUntilNextTrigger
		}
	}
	return err
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
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	paceDir := omiPaceDir(state.cfg)
	return sched.AddJob(scheduler.Job{
		Name:        jobName,
		Schedule:    source.Schedule,
		Preemptible: true,
		MaxRuntime:  omiPassRuntime,
		Run: invocationBoundJobRun(state, func(ctx context.Context) error {
			return runConfiguredOmiSync(ctx, st, paceDir, source)
		}),
	})
}
