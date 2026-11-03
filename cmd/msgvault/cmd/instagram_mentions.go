package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

func resolveInstagramMentionsProfiles(args []string, cfg *config.Config) ([]config.InstagramCommentsSource, error) {
	profiles, err := resolveInstagramCommentsProfiles(args, cfg)
	if err != nil {
		if len(args) > 0 {
			return nil, opserr.NotFound(err)
		}
		return nil, &mentionsCLIError{err, 5, "configure [[instagram_comments]] with mentions = true, then restart the daemon"}
	}
	var selected []config.InstagramCommentsSource
	for _, profile := range profiles {
		if profile.Mentions {
			selected = append(selected, profile)
		} else if len(args) > 0 {
			return nil, opserr.Invalid(fmt.Errorf("instagram profile %q does not enable mentions", profile.Identifier))
		}
	}
	if len(selected) == 0 {
		return nil, opserr.Invalid(errors.New("no [[instagram_comments]] profile enables mentions"))
	}
	return selected, nil
}

func registerInstagramMentionsProfile(ctx context.Context, st *store.Store, source config.InstagramCommentsSource) (*store.Source, error) {
	if !source.Mentions {
		return nil, opserr.Invalid(fmt.Errorf("instagram profile %q does not enable mentions", source.Identifier))
	}
	client, err := configuredInstagramCommentsClient(source)
	if err != nil {
		return nil, &mentionsCLIError{err, 7, "set the profile's token_env variable in the daemon environment, then restart the daemon"}
	}
	src, err := meta.NewMentionImporter(st, client).Register(ctx, source.AppID, source.PageID, source.InstagramAccountID)
	return src, mentionsRuntimeError(err)
}

func importInstagramMentionsProfile(ctx context.Context, st *store.Store, source config.InstagramCommentsSource, options meta.ImportOptions) (sum *meta.ImportSummary, err error) {
	defer func() {
		_ = recordSyncAttempt(ctx, meta.MentionsSourceType, meta.InstagramSourceIdentifier(source.AppID, source.PageID, source.InstagramAccountID), err)
	}()
	if !source.Mentions {
		return nil, opserr.Invalid(fmt.Errorf("instagram profile %q does not enable mentions", source.Identifier))
	}
	client, err := configuredInstagramCommentsClient(source)
	if err != nil {
		return nil, &mentionsCLIError{err, 7, "set the profile's token_env variable in the daemon environment, then restart the daemon"}
	}
	sum, err = meta.NewMentionImporter(st, client).ImportTags(ctx, source.AppID, source.PageID, source.InstagramAccountID, options)
	return sum, mentionsRuntimeError(err)
}

func runConfiguredInstagramMentionsSync(ctx context.Context, st *store.Store, source config.InstagramCommentsSource) error {
	sum, err := importInstagramMentionsProfile(ctx, st, source, meta.ImportOptions{})
	if sum == nil {
		return err
	}
	job, _ := api.SchedulerJobNameForSource(meta.MentionsSourceType, meta.InstagramSourceIdentifier(source.AppID, source.PageID, source.InstagramAccountID))
	return errors.Join(err, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), job))
}

func registerScheduledInstagramMentionsJob(sched *scheduler.Scheduler, source config.InstagramCommentsSource, maintenance *attachmentMaintenance, run func(context.Context) error) error {
	job, _ := api.SchedulerJobNameForSource(meta.MentionsSourceType, meta.InstagramSourceIdentifier(source.AppID, source.PageID, source.InstagramAccountID))
	return sched.AddJob(scheduler.Job{Name: job, Schedule: source.Schedule, Run: func(ctx context.Context) error {
		return runScheduledSource(ctx, maintenance, false, run)
	}})
}

func newAddInstagramMentionsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "add [identifier]", Short: "Register an Instagram mentions source",
		Long: "Verify the Page and linked professional account, then register profiles with mentions enabled. With no identifier, select all opted-in profiles. The token comes from the named daemon environment variable.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return &mentionsCLIError{errors.New("configuration is unavailable"), 5, "configure an Instagram comments profile with mentions = true, then restart the daemon"}
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			profiles, err := resolveInstagramMentionsProfiles(args, state.cfg)
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
				source, err := registerInstagramMentionsProfile(cmd.Context(), st, profile)
				if err != nil {
					errs = append(errs, fmt.Errorf("instagram profile %s: %w", profile.Identifier, err))
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered Instagram mentions for account %s for %s (source %d)\n", profile.InstagramAccountID, profile.Identifier, source.ID)
			}
			return errors.Join(errors.Join(errs...), runPostSourceCreateMigrationsForInvocation(st, state))
		},
	}
}

func newSyncInstagramMentionsCmd() *cobra.Command {
	options := meta.ImportOptions{MaxRequests: meta.DefaultMaxRequests}
	cmd := &cobra.Command{
		Use: "sync [identifier]", Short: "Archive tagged Instagram media",
		Long: "Poll tagged media for registered profiles with mentions enabled. Caption and comment mentions need the webhook integration. Tokens come from the daemon environment. Partial writes stay searchable and resume next time; inaccessible objects never delete archived history.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if options.MaxRequests <= 0 {
				return opserr.Invalid(errors.New("--max-requests must be positive"))
			}
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return &mentionsCLIError{errors.New("configuration is unavailable"), 5, "configure an Instagram comments profile with mentions = true, then restart the daemon"}
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			profiles, err := resolveInstagramMentionsProfiles(args, state.cfg)
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving Instagram tags checkpoints...")
			defer stop()
			var errs []error
			for _, profile := range profiles {
				if ctx.Err() != nil {
					errs = append(errs, ctx.Err())
					break
				}
				sum, err := importInstagramMentionsProfile(ctx, st, profile, options)
				if sum != nil {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Instagram mentions %s: %d messages processed (%d added), %d requests\n", profile.Identifier, sum.MessagesProcessed, sum.MessagesAdded, sum.Requests)
					if sum.Partial {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Partial coverage; unfinished tags resume on the next sync.")
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
	cmd.Flags().IntVar(&options.MaxRequests, "max-requests", meta.DefaultMaxRequests, "maximum API reads per account; at least 3 needed to collect tags")
	cmd.Flags().BoolVar(&options.Full, "full", false, "restart selected source scans with fresh cursors; preserve and update archived rows")
	return cmd
}

func init() {
	registerMetaSourceProvider("instagram-mentions", "Archive Instagram mentions and tags", newAddInstagramMentionsCmd(), addManualSyncCacheFlags(newSyncInstagramMentionsCmd()))
}

// These interfaces carry classification and recovery through #1606's renderer.
type mentionsCLIError struct {
	err  error
	exit int
	hint string
}

func (e *mentionsCLIError) Error() string { return e.err.Error() }
func (e *mentionsCLIError) Unwrap() error { return e.err }
func (e *mentionsCLIError) ExitCode() int { return e.exit }
func (e *mentionsCLIError) Hint() string  { return e.hint }

func mentionsRuntimeError(err error) error {
	if err == nil {
		return nil
	}
	var provider *meta.APIError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, store.ErrSourceNotFound):
		return &mentionsCLIError{err, 5, "run 'msgvault source instagram-mentions add <identifier>' before syncing"}
	case errors.Is(err, store.ErrSyncAlreadyActive), errors.Is(err, store.ErrSourceRetired):
		return &mentionsCLIError{err, 6, "wait for the active sync or choose an active source, then retry"}
	case errors.As(err, &provider):
		return &mentionsCLIError{err, 7, "check the profile's daemon environment token and Instagram permissions, then retry"}
	default:
		return err
	}
}
