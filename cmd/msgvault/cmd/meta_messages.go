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

// inboxProfile is a [[facebook_messages]] or [[instagram_messages]] profile.
type inboxProfile struct {
	identifier, tokenEnv, schedule string
	enabled                        bool
	inbox                          meta.Inbox
}

func facebookInboxProfiles(cfg *config.Config) []inboxProfile {
	var out []inboxProfile
	for _, p := range cfg.FacebookMessages {
		out = append(out, inboxProfile{p.Identifier, p.TokenEnv, p.Schedule, p.Enabled, meta.Inbox{App: p.AppID, Page: p.PageID}})
	}
	return out
}

func instagramInboxProfiles(cfg *config.Config) []inboxProfile {
	var out []inboxProfile
	for _, p := range cfg.InstagramMessages {
		out = append(out, inboxProfile{p.Identifier, p.TokenEnv, p.Schedule, p.Enabled, meta.Inbox{App: p.AppID, Page: p.PageID, Account: p.InstagramAccountID}})
	}
	return out
}

// inboxProvider is one inbox source command group.
type inboxProvider struct {
	name, section string
	profiles      func(*config.Config) []inboxProfile
}

var (
	facebookInbox  = inboxProvider{"facebook-messages", "facebook_messages", facebookInboxProfiles}
	instagramInbox = inboxProvider{"instagram-messages", "instagram_messages", instagramInboxProfiles}
)

func (p inboxProvider) resolve(args []string, cfg *config.Config) ([]inboxProfile, error) {
	profiles := p.profiles(cfg)
	if len(args) == 0 {
		if len(profiles) == 0 {
			return nil, &sourceCommandError{err: fmt.Errorf("configure [[%s]] first", p.section), code: 5, hint: fmt.Sprintf("configure a profile, then run 'msgvault source %s add <identifier>'", p.name)}
		}
		return profiles, nil
	}
	for _, profile := range profiles {
		if strings.EqualFold(profile.identifier, args[0]) {
			return []inboxProfile{profile}, nil
		}
	}
	return nil, opserr.NotFound(fmt.Errorf("no [[%s]] profile with identifier %q", p.section, args[0]))
}

func (profile inboxProfile) client() (*meta.Client, error) {
	token := os.Getenv(profile.tokenEnv)
	if strings.TrimSpace(token) == "" {
		return nil, &sourceCommandError{err: fmt.Errorf("profile %q needs %s in the daemon environment", profile.identifier, profile.tokenEnv), code: 7, hint: "set the named credential in the daemon environment and restart the daemon"}
	}
	client, err := meta.NewClient(token)
	if err != nil {
		return nil, &sourceCommandError{err: err, code: 7, hint: "check the named credential in the daemon environment and restart the daemon"}
	}
	return client, nil
}

func (p inboxProvider) importProfile(ctx context.Context, st *store.Store, profile inboxProfile, options meta.ImportOptions) (sum *meta.ImportSummary, err error) {
	defer func() { _ = recordSyncAttempt(ctx, profile.inbox.SourceType(), profile.inbox.Identifier(), err) }()
	client, err := profile.client()
	if err != nil {
		return nil, err
	}
	sum, err = meta.NewInboxImporter(st, client).Import(ctx, profile.inbox, options)
	return sum, metaSourceStateError(err, p.name)
}

// scheduleJobs adds a sync job for each enabled profile with a schedule.
func (p inboxProvider) scheduleJobs(sched *scheduler.Scheduler, cfg *config.Config, st *store.Store, maintenance *attachmentMaintenance, inv *invocation) []error {
	var errs []error
	for _, profile := range p.profiles(cfg) {
		if !profile.enabled || profile.schedule == "" {
			continue
		}
		job, _ := api.SchedulerJobNameForSource(profile.inbox.SourceType(), profile.inbox.Identifier())
		run := invocationBoundJobRun(inv, func(ctx context.Context) error {
			sum, err := p.importProfile(ctx, st, profile, meta.ImportOptions{})
			if sum == nil {
				return err
			}
			return errors.Join(err, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), job))
		})
		err := sched.AddJob(scheduler.Job{Name: job, Schedule: profile.schedule, Run: func(ctx context.Context) error {
			return runScheduledSource(ctx, maintenance, false, run)
		}})
		if err != nil {
			errs = append(errs, fmt.Errorf("schedule %s profile %s: %w", p.section, profile.identifier, err))
		}
	}
	return errs
}

func (p inboxProvider) addCmd(short, long string) *cobra.Command {
	return &cobra.Command{
		Use: "add [identifier]", Short: short, Long: long,
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			profiles, err := p.resolve(args, state.cfg)
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
				client, err := profile.client()
				var source *store.Source
				if err == nil {
					source, err = meta.NewInboxImporter(st, client).Register(cmd.Context(), profile.inbox)
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("profile %s: %w", profile.identifier, metaSourceStateError(err, p.name)))
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered %s inbox for %s (source %d)\n", source.DisplayName.String, profile.identifier, source.ID)
			}
			return errors.Join(errors.Join(errs...), runPostSourceCreateMigrationsForInvocation(cmd.Context(), st, state))
		},
	}
}

func (p inboxProvider) syncCmd(short, long string) *cobra.Command {
	options := meta.ImportOptions{MaxRequests: meta.DefaultMaxRequests}
	cmd := &cobra.Command{
		Use: "sync [identifier]", Short: short, Long: long,
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
			profiles, err := p.resolve(args, state.cfg)
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving inbox checkpoints...")
			defer stop()
			var errs []error
			for _, profile := range profiles {
				if ctx.Err() != nil {
					errs = append(errs, ctx.Err())
					break
				}
				sum, err := p.importProfile(ctx, st, profile, options)
				if sum != nil {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %d messages processed (%d added, %d updated), %d requests\n", profile.identifier, sum.MessagesProcessed, sum.MessagesAdded, sum.MessagesUpdated, sum.Requests)
					if sum.Partial {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Partial coverage; the next sync resumes where this one stopped.")
					}
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("profile %s: %w", profile.identifier, err))
				}
			}
			// Refresh accepted partial writes even after cancellation/provider errors.
			return errors.Join(errors.Join(errs...), rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state))
		},
	}
	cmd.Flags().IntVar(&options.MaxRequests, "max-requests", meta.DefaultMaxRequests, "maximum API reads per inbox; each reads up to 25 conversations")
	cmd.Flags().BoolVar(&options.Full, "full", false, "restart from the most recent conversations; preserve and update archived rows")
	return cmd
}

func init() {
	registerMetaSourceProvider(facebookInbox.name, "Archive a Facebook Page's Messenger inbox",
		facebookInbox.addCmd("Register a Facebook Page inbox",
			"Verify the Page token, then register [[facebook_messages]] profiles. With no identifier, register every profile. The Page token is read only from the named daemon environment variable."),
		addManualSyncCacheFlags(facebookInbox.syncCmd("Archive Messenger conversations with the Page",
			"Archive the Page's Messenger conversations as chats, with up to the 20 most recent messages of each. Progress resumes on the next sync. Tokens come from the daemon environment.")))
	registerMetaSourceProvider(instagramInbox.name, "Archive a linked Instagram account's inbox",
		instagramInbox.addCmd("Register an Instagram account inbox",
			"Verify the Page and its linked professional account, then register [[instagram_messages]] profiles. With no identifier, register every profile. The Page token is read only from the named daemon environment variable."),
		addManualSyncCacheFlags(instagramInbox.syncCmd("Archive Instagram direct messages",
			"Archive the linked account's Instagram conversations as chats, with up to the 20 most recent messages of each. Progress resumes on the next sync. Tokens come from the daemon environment.")))
}
