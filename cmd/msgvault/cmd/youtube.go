package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/youtube"
)

func configuredYouTubeClient(ctx context.Context, src config.YouTubeSource) (*youtube.Client, error) {
	for _, key := range []string{src.ClientIDEnv, src.ClientSecretEnv, src.TokenEnv} {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			return nil, fmt.Errorf("YouTube profile %q needs %s in the daemon environment", src.Identifier, key)
		}
	}
	return youtube.NewClient(ctx, os.Getenv(src.ClientIDEnv), os.Getenv(src.ClientSecretEnv), os.Getenv(src.TokenEnv))
}

func resolveYouTubeProfiles(args []string, cfg *config.Config) ([]config.YouTubeSource, error) {
	if len(args) > 0 {
		src := cfg.GetYouTubeSource(args[0])
		if src == nil {
			return nil, fmt.Errorf("no [[youtube]] profile with identifier %q", args[0])
		}
		return []config.YouTubeSource{*src}, nil
	}
	if len(cfg.YouTube) == 0 {
		return nil, errors.New("no [[youtube]] profiles configured; set channel_id and named OAuth environment variables")
	}
	return cfg.YouTube, nil
}

func syncYouTubeProfile(ctx context.Context, st *store.Store, src config.YouTubeSource, budget int) (*youtube.ImportSummary, error) {
	registered, err := st.GetSourceByTypeAndIdentifier(ctx, youtube.SourceType, youtube.SourceIdentifier(src.ChannelID))
	if err != nil {
		return nil, err
	}
	if registered.MergedIntoSourceID != 0 {
		return nil, store.ErrSourceRetired
	}
	client, err := configuredYouTubeClient(ctx, src)
	if err != nil {
		return nil, err
	}
	return youtube.NewImporter(st, client).Import(ctx, youtube.ImportOptions{ChannelID: src.ChannelID, MaxRequests: budget})
}

func newAddYouTubeCmd() *cobra.Command {
	return &cobra.Command{Use: "add [identifier]", Short: "Register an explicitly configured owned YouTube channel", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		profiles, err := resolveYouTubeProfiles(args, state.cfg)
		if err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		var errs []error
		for _, src := range profiles {
			client, err := configuredYouTubeClient(cmd.Context(), src)
			if err == nil {
				_, err = youtube.NewImporter(st, client).Register(cmd.Context(), src.ChannelID)
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered YouTube channel %s\n", src.ChannelID)
		}
		errs = append(errs, runPostSourceCreateMigrationsForInvocation(cmd.Context(), st, state))
		return errors.Join(errs...)
	}}
}

func newSyncYouTubeCmd() *cobra.Command {
	budget := 1000
	cmd := &cobra.Command{Use: "sync [identifier]", Short: "Poll registered YouTube videos, comments and replies", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if budget < 2 || budget > 100000 {
			return usageErr(cmd, errors.New("--max-requests must be between 2 and 100000"))
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		profiles, err := resolveYouTubeProfiles(args, state.cfg)
		if err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving YouTube progress...")
		defer stop()
		var errs []error
		for _, src := range profiles {
			sum, err := syncYouTubeProfile(ctx, st, src, budget)
			if errors.Is(err, store.ErrSourceRetired) {
				continue
			}
			if sum != nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "YouTube %s: %d messages processed, %d API requests, partial=%t\n", src.Identifier, sum.MessagesProcessed, sum.Requests, sum.Partial)
			}
			errs = append(errs, recordSyncAttempt(ctx, youtube.SourceType, youtube.SourceIdentifier(src.ChannelID), err))
			if ctx.Err() != nil {
				break
			}
		}
		errs = append(errs, rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state))
		return errors.Join(errs...)
	}}
	cmd.Flags().IntVar(&budget, "max-requests", 1000, "maximum Data API requests including channel verification (minimum 2)")
	return cmd
}

func registerScheduledYouTubeJob(sched *scheduler.Scheduler, src config.YouTubeSource, maintenance *attachmentMaintenance, run func(context.Context) error) error {
	job, ok := api.YouTubeJobNameForChannel(src.ChannelID)
	if !ok {
		return errors.New("invalid YouTube scheduler channel")
	}
	return sched.AddJob(scheduler.Job{Name: job, Schedule: src.Schedule, Run: func(ctx context.Context) error { return runScheduledSource(ctx, maintenance, true, run) }})
}

func runConfiguredYouTubeSync(ctx context.Context, st *store.Store, src config.YouTubeSource) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	job, ok := api.YouTubeJobNameForChannel(src.ChannelID)
	if !ok {
		return errors.New("invalid YouTube scheduler channel")
	}
	_, err := syncYouTubeProfile(ctx, st, src, 1000)
	if errors.Is(err, store.ErrSourceRetired) {
		return nil
	}
	_ = recordSyncAttempt(ctx, youtube.SourceType, youtube.SourceIdentifier(src.ChannelID), err)
	return errors.Join(err, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), job))
}

func init() {
	registerMetaSourceProvider("youtube", "Archive YouTube comments", newAddYouTubeCmd(), addManualSyncCacheFlags(newSyncYouTubeCmd()))
}
