package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

// metaMessagesError exposes the error contract understood by the grouped CLI
// classifier without depending on its unreleased diagnostic renderer.
type metaMessagesError struct {
	err  error
	exit int
	hint string
}

func (e *metaMessagesError) Error() string { return e.err.Error() }
func (e *metaMessagesError) Unwrap() error { return e.err }
func (e *metaMessagesError) ExitCode() int { return e.exit }
func (e *metaMessagesError) Hint() string  { return e.hint }
func messagesError(err error, exit int, hint string) error {
	if err == nil {
		return nil
	}
	return &metaMessagesError{err: err, exit: exit, hint: hint}
}
func classifyMetaMessagesError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[interface {
		error
		ExitCode() int
	}](err); ok {
		return err
	}

	var provider *meta.APIError
	var network net.Error
	switch {
	case errors.Is(err, store.ErrSourceNotFound):
		return messagesError(err, 4, "register the configured inbox with 'msgvault source <provider>-messages add <identifier>'")
	case errors.Is(err, store.ErrSyncAlreadyActive), errors.Is(err, store.ErrSourceRetired):
		return messagesError(err, 6, "wait for the active sync or select an active inbox source")
	case errors.As(err, &provider), errors.As(err, &network):
		return messagesError(err, 7, "check the configured inbox token, permissions and provider availability")
	default:
		return err
	}
}
func messagesArgs(cmd *cobra.Command, args []string) error {
	return messagesError(cobra.MaximumNArgs(1)(cmd, args), 2, "run '"+cmd.CommandPath()+" --help' for flags and examples")
}

func messagesPlatformName(p meta.DMPlatform) string {
	if p == meta.PlatformInstagram {
		return "instagram"
	}
	return "facebook"
}
func messagesSource(p meta.DMPlatform, profile config.MetaMessagesSource) meta.DMSource {
	return meta.DMSource{Platform: p, AppID: profile.AppID, PageID: profile.PageID, IGID: profile.InstagramAccountID}
}
func resolveMetaMessagesProfiles(p meta.DMPlatform, args []string, cfg *config.Config) ([]config.MetaMessagesSource, error) {
	profiles := cfg.FacebookMessages
	if p == meta.PlatformInstagram {
		profiles = cfg.InstagramMessages
	}
	if len(args) > 0 {
		for _, profile := range profiles {
			if strings.EqualFold(profile.Identifier, args[0]) {
				return []config.MetaMessagesSource{profile}, nil
			}
		}
		return nil, messagesError(fmt.Errorf("no [[%s_messages]] profile with identifier %q", messagesPlatformName(p), args[0]), 4, "select an identifier from the configured inbox profiles")
	}
	if len(profiles) == 0 {
		return nil, messagesError(fmt.Errorf("configure [[%s_messages]] with identifier, app_id, page_id and token_env first", messagesPlatformName(p)), 5, "add an inbox profile to config.toml and restart the daemon")
	}
	return profiles, nil
}
func configuredMetaMessagesClient(p meta.DMPlatform, profile config.MetaMessagesSource) (*meta.Client, error) {
	token := os.Getenv(profile.TokenEnv)
	if strings.TrimSpace(token) == "" {
		return nil, messagesError(fmt.Errorf("%s messages profile %q needs %s in the daemon environment", messagesPlatformName(p), profile.Identifier, profile.TokenEnv), 7, "set the configured token environment variable and restart the daemon")
	}
	client, err := meta.NewClient(token)
	return client, messagesError(err, 7, "set a valid token in the configured environment variable and restart the daemon")
}
func registerMetaMessagesProfile(ctx context.Context, st *store.Store, p meta.DMPlatform, profile config.MetaMessagesSource) (*store.Source, error) {
	client, err := configuredMetaMessagesClient(p, profile)
	if err != nil {
		return nil, err
	}
	return meta.NewDMImporter(st, client).Register(ctx, messagesSource(p, profile))
}
func importMetaMessagesProfile(ctx context.Context, st *store.Store, p meta.DMPlatform, profile config.MetaMessagesSource, options meta.ImportOptions) (sum *meta.DMImportSummary, err error) {
	defer func() { _ = recordSyncAttempt(ctx, p.SourceType(), messagesSource(p, profile).Identifier(), err) }()
	client, err := configuredMetaMessagesClient(p, profile)
	if err != nil {
		return nil, err
	}
	return meta.NewDMImporter(st, client).Import(ctx, messagesSource(p, profile), options)
}
func runConfiguredMetaMessagesSync(ctx context.Context, st *store.Store, p meta.DMPlatform, profile config.MetaMessagesSource) error {
	sum, err := importMetaMessagesProfile(ctx, st, p, profile, meta.ImportOptions{})
	if sum == nil {
		return err
	}
	job, _ := api.SchedulerJobNameForSource(p.SourceType(), messagesSource(p, profile).Identifier())
	return errors.Join(err, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), job))
}
func registerScheduledMetaMessagesJob(sched *scheduler.Scheduler, p meta.DMPlatform, profile config.MetaMessagesSource, maintenance *attachmentMaintenance, run func(context.Context) error) error {
	job, ok := api.SchedulerJobNameForSource(p.SourceType(), messagesSource(p, profile).Identifier())
	if !ok {
		return errors.New("invalid inbox scheduler identity")
	}
	return sched.AddJob(scheduler.Job{Name: job, Schedule: profile.Schedule, Run: func(ctx context.Context) error {
		return runScheduledSource(ctx, maintenance, false, run)
	}})
}
func newAddMetaMessagesCmd(p meta.DMPlatform) *cobra.Command {
	label := messagesPlatformName(p)
	return &cobra.Command{
		Use: "add [identifier]", Short: "Register a configured " + label + " inbox for message sync",
		Long: "With no identifier, register all configured inbox profiles. Tokens come from the named daemon environment variable. This does not subscribe an app or change routing.",
		Args: messagesArgs, RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return messagesError(errors.New("configuration is unavailable"), 5, "configure msgvault and restart the daemon")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			profiles, err := resolveMetaMessagesProfiles(p, args, state.cfg)
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
				source, err := registerMetaMessagesProfile(cmd.Context(), st, p, profile)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s messages profile %s: %w", label, profile.Identifier, classifyMetaMessagesError(err)))
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered %s inbox for %s (source %d)\n", label, profile.Identifier, source.ID)
			}
			return errors.Join(errors.Join(errs...), runPostSourceCreateMigrationsForInvocation(st, state))
		},
	}
}
func newSyncMetaMessagesCmd(p meta.DMPlatform) *cobra.Command {
	label := messagesPlatformName(p)
	options := meta.ImportOptions{MaxRequests: meta.DefaultMaxRequests}
	cmd := &cobra.Command{
		Use: "sync [identifier]", Short: "Archive recent " + label + " inbox messages",
		Long: "Requires registered inbox profiles. Each thread exposes at most its 20 most recent messages. Partial writes stay searchable. Tokens come from the named daemon environment variable; live coverage requires the Meta webhook receiver.",
		Args: messagesArgs, RunE: func(cmd *cobra.Command, args []string) error {
			if options.MaxRequests <= 0 {
				return messagesError(errors.New("--max-requests must be positive"), 3, "set --max-requests to a positive integer")
			}
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return messagesError(errors.New("configuration is unavailable"), 5, "configure msgvault and restart the daemon")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			profiles, err := resolveMetaMessagesProfiles(p, args, state.cfg)
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
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
				sum, err := importMetaMessagesProfile(ctx, st, p, profile, options)
				if sum != nil {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s: threads=%d messages_added=%d requests=%d partial=%t\nNote: %s\n", label, profile.Identifier, sum.Threads, sum.MessagesAdded, sum.Requests, sum.Partial, sum.Note)
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("%s messages profile %s: %w", label, profile.Identifier, classifyMetaMessagesError(err)))
				}
			}
			return errors.Join(errors.Join(errs...), rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state))
		},
	}
	cmd.Flags().IntVar(&options.MaxRequests, "max-requests", meta.DefaultMaxRequests, "maximum API reads per inbox, including identity checks")
	cmd.Flags().BoolVar(&options.Full, "full", false, "restart the inbox scan with fresh cursors; preserve archived history")
	return cmd
}
func registerMetaMessagesCommands(root *cobra.Command) {
	const sourceName = "source"
	var source *cobra.Command
	for _, child := range root.Commands() {
		if child.Name() == sourceName {
			source = child
			break
		}
	}
	if source == nil {
		source = &cobra.Command{Use: sourceName, Short: "Add and sync provider sources"}
		root.AddCommand(source)
	}
	for _, p := range []meta.DMPlatform{meta.PlatformMessenger, meta.PlatformInstagram} {
		provider := &cobra.Command{Use: messagesPlatformName(p) + "-messages", Short: "Register and archive " + messagesPlatformName(p) + " inbox messages"}
		provider.AddCommand(newAddMetaMessagesCmd(p), addManualSyncCacheFlags(newSyncMetaMessagesCmd(p)))
		source.AddCommand(provider)
	}
}
func init() {
	additionalWebhookPageProfiles = func(cfg *config.Config) []webhookPageProfile {
		profiles := make([]webhookPageProfile, 0, len(cfg.FacebookMessages)+len(cfg.InstagramMessages))
		for _, p := range cfg.FacebookMessages {
			profiles = append(profiles, webhookPageProfile{Kind: meta.SourceTypeFacebookMessages, Identifier: p.Identifier, AppID: p.AppID, PageID: p.PageID, TokenEnv: p.TokenEnv})
		}
		for _, p := range cfg.InstagramMessages {
			profiles = append(profiles, webhookPageProfile{Kind: meta.SourceTypeInstagramMessages, Identifier: p.Identifier, AppID: p.AppID, PageID: p.PageID, TokenEnv: p.TokenEnv})
		}
		return profiles
	}
	registerMetaMessagesCommands(rootCmd)
}
