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

var (
	rebuildFacebookCommentsCacheAfterWrite         = rebuildCacheAfterManualSync
	rebuildFacebookCommentsCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

type missingMetaTokenError struct {
	err error
}

func (e *missingMetaTokenError) Error() string { return e.err.Error() }
func (e *missingMetaTokenError) Unwrap() error { return e.err }

func metaTokenMissingErrorf(format string, args ...any) error {
	return &missingMetaTokenError{err: fmt.Errorf(format, args...)}
}

func withMetaProfileContext(prefix string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*missingMetaTokenError](err); ok {
		return err
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

func resolveFacebookCommentsProfiles(args []string, cfg *config.Config) ([]config.FacebookCommentsSource, error) {
	if len(args) > 0 {
		source := cfg.GetFacebookCommentsSource(args[0])
		if source == nil {
			return nil, opserr.NotFound(fmt.Errorf("no [[facebook_comments]] profile with identifier %q", args[0]))
		}
		return []config.FacebookCommentsSource{*source}, nil
	}
	if len(cfg.FacebookComments) == 0 {
		return nil, &sourceCommandError{err: errors.New("configure [[facebook_comments]] with identifier, app_id, page_id and token_env first"), code: 5, hint: "configure a profile, then run 'msgvault source facebook-comments add <identifier>'"}
	}
	return cfg.FacebookComments, nil
}

func configuredFacebookCommentsClient(source config.FacebookCommentsSource) (*meta.Client, error) {
	token := os.Getenv(source.TokenEnv)
	if strings.TrimSpace(token) == "" {
		return nil, &sourceCommandError{err: metaTokenMissingErrorf("facebook profile %q needs %s in the daemon environment", source.Identifier, source.TokenEnv), code: 7, hint: "set the named credential in the daemon environment and restart the daemon"}
	}
	client, err := meta.NewClient(token)
	if err != nil {
		return nil, &sourceCommandError{err: err, code: 7, hint: "check the named credential in the daemon environment and restart the daemon"}
	}
	return client, nil
}

func registerFacebookCommentsProfile(ctx context.Context, st *store.Store, source config.FacebookCommentsSource) (*store.Source, error) {
	client, err := configuredFacebookCommentsClient(source)
	if err != nil {
		return nil, err
	}
	registered, err := meta.NewImporter(st, client).Register(ctx, source.AppID, source.PageID)
	return registered, metaSourceStateError(err, "facebook-comments")
}

func importFacebookCommentsProfile(ctx context.Context, st *store.Store, source config.FacebookCommentsSource, options meta.ImportOptions) (sum *meta.ImportSummary, err error) {
	defer func() {
		_ = recordSyncAttempt(ctx, meta.SourceType, meta.SourceIdentifier(source.AppID, source.PageID), err)
	}()
	client, err := configuredFacebookCommentsClient(source)
	if err != nil {
		return nil, err
	}
	sum, err = meta.NewImporter(st, client).Import(ctx, source.AppID, source.PageID, options)
	return sum, metaSourceStateError(err, "facebook-comments")
}

func runConfiguredFacebookCommentsSync(ctx context.Context, st *store.Store, source config.FacebookCommentsSource) error {
	sum, err := importFacebookCommentsProfile(ctx, st, source, meta.ImportOptions{})
	if sum == nil {
		return err
	}
	job, _ := api.SchedulerJobNameForSource(meta.SourceType, meta.SourceIdentifier(source.AppID, source.PageID))
	return errors.Join(err, rebuildFacebookCommentsCacheAfterScheduledSync(context.WithoutCancel(ctx), job))
}

func registerScheduledFacebookCommentsJob(sched *scheduler.Scheduler, source config.FacebookCommentsSource, maintenance *attachmentMaintenance, run func(context.Context) error) error {
	job, _ := api.SchedulerJobNameForSource(meta.SourceType, meta.SourceIdentifier(source.AppID, source.PageID))
	return sched.AddJob(scheduler.Job{Name: job, Schedule: source.Schedule, Run: func(ctx context.Context) error {
		return runScheduledSource(ctx, maintenance, false, run)
	}})
}

func newAddFacebookCommentsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "add [identifier]", Short: "Register a configured Facebook Page for comment sync",
		Long: "Omit identifier to register all [[facebook_comments]] profiles. Page tokens come from the named daemon environment variable.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			profiles, err := resolveFacebookCommentsProfiles(args, state.cfg)
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
				source, err := registerFacebookCommentsProfile(cmd.Context(), st, profile)
				if err != nil {
					errs = append(errs, withMetaProfileContext("facebook profile "+profile.Identifier, err))
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered Facebook Page %s for %s (source %d)\n", profile.PageID, profile.Identifier, source.ID)
			}
			return errors.Join(errors.Join(errs...), runPostSourceCreateMigrationsForInvocation(st, state))
		},
	}
}

func newSyncFacebookCommentsCmd() *cobra.Command {
	options := meta.ImportOptions{MaxRequests: meta.DefaultMaxRequests}
	cmd := &cobra.Command{
		Use: "sync [identifier]", Short: "Archive Facebook Page comments and nested replies",
		Long: "Includes available organic, visitor and ad Facebook Page posts. Tokens come from the daemon environment. Partial imports stay searchable and resume on the next sync; missing or inaccessible objects never delete archived history.",
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
			profiles, err := resolveFacebookCommentsProfiles(args, state.cfg)
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving Facebook comment checkpoints...")
			defer stop()
			var errs []error
			for _, profile := range profiles {
				if ctx.Err() != nil {
					errs = append(errs, ctx.Err())
					break
				}
				sum, err := importFacebookCommentsProfile(ctx, st, profile, options)
				if sum != nil {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Facebook %s: %d messages processed (%d added), %d requests\n", profile.Identifier, sum.MessagesProcessed, sum.MessagesAdded, sum.Requests)
					if sum.Partial {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Partial coverage; sync again to continue.")
					}
				}
				if err != nil {
					errs = append(errs, withMetaProfileContext("facebook profile "+profile.Identifier, err))
				}
			}
			// Refresh accepted partial writes even after cancellation/provider errors.
			return errors.Join(errors.Join(errs...), rebuildFacebookCommentsCacheAfterWrite(state.cfg.DatabaseDSN(), state))
		},
	}
	cmd.Flags().IntVar(&options.MaxRequests, "max-requests", meta.DefaultMaxRequests, "maximum API reads per Page, including identity checks")
	cmd.Flags().BoolVar(&options.Full, "full", false, "rescan selected sources; preserve and update archived messages")
	return cmd
}

func init() {
	registerMetaSourceProvider("facebook-comments", "Archive comments from a configured Facebook Page", newAddFacebookCommentsCmd(), addManualSyncCacheFlags(newSyncFacebookCommentsCmd()))
}
