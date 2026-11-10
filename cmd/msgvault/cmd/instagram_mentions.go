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

// resolveInstagramMentionsProfiles selects [[instagram_comments]] profiles
// that set mentions = true.
func resolveInstagramMentionsProfiles(args []string, cfg *config.Config) ([]config.InstagramCommentsSource, error) {
	profiles, err := resolveInstagramCommentsProfiles(args, cfg)
	if err != nil {
		return nil, err
	}
	var selected []config.InstagramCommentsSource
	for _, profile := range profiles {
		if profile.Mentions {
			selected = append(selected, profile)
		}
	}
	if len(selected) == 0 {
		return nil, &sourceCommandError{err: errors.New("no selected [[instagram_comments]] profile sets mentions = true"), code: 5, hint: "set mentions = true on the profile and restart the daemon"}
	}
	return selected, nil
}

func importInstagramMentionsProfile(ctx context.Context, st *store.Store, source config.InstagramCommentsSource, options meta.ImportOptions) (sum *meta.ImportSummary, err error) {
	defer func() {
		_ = recordSyncAttempt(ctx, meta.MentionsSourceType, meta.InstagramSourceIdentifier(source.AppID, source.PageID, source.InstagramAccountID), err)
	}()
	client, err := configuredInstagramCommentsClient(source)
	if err != nil {
		return nil, err
	}
	sum, err = meta.NewMentionImporter(st, client).ImportTags(ctx, source.AppID, source.PageID, source.InstagramAccountID, options)
	return sum, metaSourceStateError(err, "instagram-mentions")
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
		Use: "add [identifier]", Short: "Register an Instagram account for tagged media",
		Long: "Verify the Page and linked professional account, then register [[instagram_comments]] profiles that set mentions = true. With no identifier, register every such profile. The Page token is read only from the named daemon environment variable.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			profiles, err := resolveInstagramMentionsProfiles(args, state.cfg)
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
			if err != nil {
				return err
			}
			defer cleanup()
			var errs []error
			for _, profile := range profiles {
				client, err := configuredInstagramCommentsClient(profile)
				var source *store.Source
				if err == nil {
					source, err = meta.NewMentionImporter(st, client).Register(cmd.Context(), profile.AppID, profile.PageID, profile.InstagramAccountID)
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("instagram profile %s: %w", profile.Identifier, metaSourceStateError(err, "instagram-mentions")))
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered Instagram mentions for account %s for %s (source %d)\n", profile.InstagramAccountID, profile.Identifier, source.ID)
			}
			return errors.Join(errors.Join(errs...), runPostSourceCreateMigrationsForInvocation(cmd.Context(), st, state))
		},
	}
}

func newSyncInstagramMentionsCmd() *cobra.Command {
	options := meta.ImportOptions{MaxRequests: meta.DefaultMaxRequests}
	cmd := &cobra.Command{
		Use: "sync [identifier]", Short: "Archive media that tags the Instagram account",
		Long: "Archive media in which other accounts tag the linked Instagram account, for registered profiles that set mentions = true. Progress resumes on the next sync. Tokens come from the daemon environment. Media that is no longer returned stays archived.",
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
			profiles, err := resolveInstagramMentionsProfiles(args, state.cfg)
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving Instagram mentions checkpoints...")
			defer stop()
			var errs []error
			for _, profile := range profiles {
				if ctx.Err() != nil {
					errs = append(errs, ctx.Err())
					break
				}
				sum, err := importInstagramMentionsProfile(ctx, st, profile, options)
				if sum != nil {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Instagram mentions %s: %d messages processed (%d added, %d updated), %d requests\n", profile.Identifier, sum.MessagesProcessed, sum.MessagesAdded, sum.MessagesUpdated, sum.Requests)
					if sum.Partial {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Partial coverage; the next sync resumes where this one stopped.")
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
	cmd.Flags().IntVar(&options.MaxRequests, "max-requests", meta.DefaultMaxRequests, "maximum API reads per account")
	cmd.Flags().BoolVar(&options.Full, "full", false, "restart from the newest tagged media; preserve and update archived rows")
	return cmd
}

func init() {
	registerMetaSourceProvider("instagram-mentions", "Archive Instagram media that tags a linked account", newAddInstagramMentionsCmd(), addManualSyncCacheFlags(newSyncInstagramMentionsCmd()))
}
