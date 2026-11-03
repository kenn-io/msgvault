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
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

func resolveInstagramCommentsProfiles(args []string, cfg *config.Config) ([]config.InstagramCommentsSource, error) {
	if len(args) > 0 {
		source := cfg.GetInstagramCommentsSource(args[0])
		if source == nil {
			return nil, opserr.NotFound(fmt.Errorf("no [[instagram_comments]] profile with identifier %q", args[0]))
		}
		return []config.InstagramCommentsSource{*source}, nil
	}
	if len(cfg.InstagramComments) == 0 {
		return nil, &sourceCommandError{err: errors.New("configure [[instagram_comments]] with identifier, app_id, page_id, instagram_account_id and token_env first"), code: 5, hint: "configure a profile, then run 'msgvault source instagram-comments add <identifier>'"}
	}
	return cfg.InstagramComments, nil
}

func configuredInstagramCommentsClient(source config.InstagramCommentsSource) (*meta.Client, error) {
	token := os.Getenv(source.TokenEnv)
	if strings.TrimSpace(token) == "" {
		return nil, &sourceCommandError{err: fmt.Errorf("instagram profile %q needs %s in the daemon environment", source.Identifier, source.TokenEnv), code: 7, hint: "set the named credential in the daemon environment and restart the daemon"}
	}
	client, err := meta.NewClient(token)
	if err != nil {
		return nil, &sourceCommandError{err: err, code: 7, hint: "check the named credential in the daemon environment and restart the daemon"}
	}
	return client, nil
}

func registerInstagramCommentsProfile(ctx context.Context, st *store.Store, source config.InstagramCommentsSource) (*store.Source, error) {
	client, err := configuredInstagramCommentsClient(source)
	if err != nil {
		return nil, err
	}
	registered, err := meta.NewImporter(st, client).RegisterInstagram(ctx, source.AppID, source.PageID, source.InstagramAccountID)
	return registered, metaSourceStateError(err, "instagram-comments")
}

func importInstagramCommentsProfile(ctx context.Context, st *store.Store, source config.InstagramCommentsSource, options meta.ImportOptions) (sum *meta.ImportSummary, err error) {
	defer func() {
		_ = recordSyncAttempt(ctx, meta.InstagramSourceType, meta.InstagramSourceIdentifier(source.AppID, source.PageID, source.InstagramAccountID), err)
	}()
	client, err := configuredInstagramCommentsClient(source)
	if err != nil {
		return nil, err
	}
	sum, err = meta.NewImporter(st, client).ImportInstagram(ctx, source.AppID, source.PageID, source.InstagramAccountID, options)
	return sum, metaSourceStateError(err, "instagram-comments")
}

func runConfiguredInstagramCommentsSync(ctx context.Context, st *store.Store, source config.InstagramCommentsSource) error {
	sum, err := importInstagramCommentsProfile(ctx, st, source, meta.ImportOptions{})
	if sum == nil {
		return err
	}
	job, _ := api.SchedulerJobNameForSource(meta.InstagramSourceType, meta.InstagramSourceIdentifier(source.AppID, source.PageID, source.InstagramAccountID))
	return errors.Join(err, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), job))
}

func registerScheduledInstagramCommentsJob(sched *scheduler.Scheduler, source config.InstagramCommentsSource, maintenance *attachmentMaintenance, run func(context.Context) error) error {
	job, _ := api.SchedulerJobNameForSource(meta.InstagramSourceType, meta.InstagramSourceIdentifier(source.AppID, source.PageID, source.InstagramAccountID))
	return sched.AddJob(scheduler.Job{Name: job, Schedule: source.Schedule, Run: func(ctx context.Context) error {
		return runScheduledSource(ctx, maintenance, false, run)
	}})
}

func newAddInstagramCommentsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "add [identifier]", Short: "Register a linked Instagram account",
		Long: "Omit identifier to register all [[instagram_comments]] profiles. Page tokens come from the named daemon environment variable.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			profiles, err := resolveInstagramCommentsProfiles(args, state.cfg)
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			var errs []error
			for _, profile := range profiles {
				source, err := registerInstagramCommentsProfile(cmd.Context(), st, profile)
				if err != nil {
					errs = append(errs, fmt.Errorf("instagram profile %s: %w", profile.Identifier, err))
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered Instagram professional account %s for %s (source %d)\n", profile.InstagramAccountID, profile.Identifier, source.ID)
			}
			return errors.Join(errors.Join(errs...), runPostSourceCreateMigrationsForInvocation(st, state))
		},
	}
}

func newSyncInstagramCommentsCmd() *cobra.Command {
	options := meta.ImportOptions{MaxRequests: meta.DefaultMaxRequests}
	cmd := &cobra.Command{
		Use: "sync [identifier]", Short: "Archive Instagram comments and replies",
		Long: "Ad-only media may be absent. Tokens come from the daemon environment. Partial imports stay searchable and resume on the next sync; missing or inaccessible objects never delete archived history.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if options.MaxRequests <= 0 {
				return opserr.Invalid(errors.New("--max-requests must be positive"))
			}
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			profiles, err := resolveInstagramCommentsProfiles(args, state.cfg)
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving Instagram comment checkpoints...")
			defer stop()
			var errs []error
			for _, profile := range profiles {
				if ctx.Err() != nil {
					errs = append(errs, ctx.Err())
					break
				}
				sum, err := importInstagramCommentsProfile(ctx, st, profile, options)
				if sum != nil {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Instagram %s: %d messages processed (%d added), %d requests\n", profile.Identifier, sum.MessagesProcessed, sum.MessagesAdded, sum.Requests)
					if sum.Partial {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Partial coverage; unfinished surfaces resume on the next sync.")
					}
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("instagram profile %s: %w", profile.Identifier, err))
				}
			}
			// Refresh accepted partial writes even after cancellation/provider errors.
			return errors.Join(errors.Join(errs...), rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state))
		},
	}
	cmd.Flags().IntVar(&options.MaxRequests, "max-requests", meta.DefaultMaxRequests, "maximum API reads per account, including Page and linkage checks")
	cmd.Flags().BoolVar(&options.Full, "full", false, "restart selected source scans with fresh cursors; preserve and update archived rows")
	return cmd
}

func init() {
	registerMetaSourceProvider("instagram-comments", "Archive comments from a linked Instagram account", newAddInstagramCommentsCmd(), addManualSyncCacheFlags(newSyncInstagramCommentsCmd()))
}
