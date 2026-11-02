package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/tiktok"
)

func configuredTikTokClient(cfg *config.Config, profile config.TikTokSource) (*tiktok.Client, error) {
	token := os.Getenv(profile.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("TikTok profile %q needs %s in the daemon environment\nhint: set the named variable and restart the daemon", profile.Identifier, profile.TokenEnv)
	}
	secret := ""
	if profile.AppSecretEnv != "" {
		secret = os.Getenv(profile.AppSecretEnv)
		if secret == "" {
			return nil, fmt.Errorf("TikTok profile needs %s in the daemon environment", profile.AppSecretEnv)
		}
	}
	return tiktok.NewManagedClient(filepath.Join(cfg.Data.DataDir, "tiktok"), profile.AppID, profile.AccountID, token, secret)
}

func selectedTikTokProfiles(cfg *config.Config, args []string) ([]config.TikTokSource, error) {
	if len(args) > 0 {
		p := cfg.GetTikTokSource(args[0])
		if p == nil {
			return nil, fmt.Errorf("no [[tiktok]] profile with identifier %q\nhint: configure the profile before adding it", args[0])
		}
		if !p.Enabled {
			return nil, errors.New("TikTok profile is disabled\nhint: set enabled = true and restart the daemon")
		}
		return []config.TikTokSource{*p}, nil
	}
	var profiles []config.TikTokSource
	for _, p := range cfg.TikTok {
		if p.Enabled {
			profiles = append(profiles, p)
		}
	}
	if len(profiles) == 0 {
		return nil, errors.New("no enabled [[tiktok]] profiles\nhint: configure identifier, app_id, account_id, token_env and enabled = true")
	}
	return profiles, nil
}

type tikTokResult struct {
	Profile  string                `json:"profile"`
	SourceID int64                 `json:"source_id,omitempty"`
	Summary  *tiktok.ImportSummary `json:"summary,omitempty"`
}

func newTikTokCommentsCmd() *cobra.Command {
	group := &cobra.Command{Use: "tiktok-comments", Short: "Archive comments on owned TikTok videos"}
	group.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if pre := cmd.Root().PersistentPreRunE; pre != nil {
			return tikTokCommandError(cmd, pre(cmd, args))
		}
		return nil
	}
	for _, operation := range []string{"add", "sync"} {
		group.AddCommand(newTikTokCommentsAction(operation))
	}
	return group
}

func tikTokCommandError(cmd *cobra.Command, err error) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	if err != nil && asJSON && !cmd.SilenceErrors {
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		_ = json.MarshalWrite(cmd.ErrOrStderr(), map[string]string{"error": err.Error()})
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	}
	return err
}
func newTikTokCommentsAction(operation string) *cobra.Command {
	options := tiktok.ImportOptions{MaxRequests: tiktok.DefaultMaxRequests}
	var asJSON bool
	command := &cobra.Command{Use: operation + " [identifier]", Short: "Register a configured TikTok account", Args: func(cmd *cobra.Command, args []string) error {
		return tikTokCommandError(cmd, cobra.MaximumNArgs(1)(cmd, args))
	}}
	command.SetFlagErrorFunc(tikTokCommandError)
	if operation == "sync" {
		command.Short = "Archive video captions, comments and replies"
		command.Flags().IntVar(&options.MaxRequests, "max-requests", tiktok.DefaultMaxRequests, "maximum API reads per account, including identity checks")
		command.Flags().BoolVar(&options.Full, "full", false, "restart traversal with fresh cursors; preserve archived rows")
		addManualSyncCacheFlags(command)
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print JSON results; errors use JSON on stderr")
	command.RunE = func(cmd *cobra.Command, args []string) (retErr error) {
		defer func() { retErr = tikTokCommandError(cmd, retErr) }()
		if options.MaxRequests <= 0 {
			return errors.New("--max-requests must be positive\nhint: use at least 2 to fetch videos")
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		profiles, err := selectedTikTokProfiles(state.cfg, args)
		if err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving TikTok checkpoints...")
		defer stop()
		var results []tikTokResult
		var errs []error
		for _, profile := range profiles {
			if ctx.Err() != nil {
				errs = append(errs, ctx.Err())
				break
			}
			client, err := configuredTikTokClient(state.cfg, profile)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			imp := tiktok.NewImporter(st, client)
			result := tikTokResult{Profile: profile.Identifier}
			if operation == "add" {
				source, addErr := imp.Register(ctx, profile.AppID, profile.AccountID)
				err = addErr
				if source != nil {
					result.SourceID = source.ID
				}
			} else {
				result.Summary, err = imp.Import(ctx, profile.AppID, profile.AccountID, options)
				_ = recordSyncAttempt(ctx, tiktok.CommentsSourceType, tiktok.SourceIdentifier(profile.AppID, profile.AccountID), err)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("TikTok profile %s: %w", profile.Identifier, err))
			}
			results = append(results, result)
			if !asJSON {
				if operation == "add" && err == nil {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered TikTok %s (source %d)\n", profile.Identifier, result.SourceID)
				}
				if sum := result.Summary; sum != nil {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "TikTok %s: %d messages processed (%d added), %d requests; partial: %t\n", profile.Identifier, sum.MessagesProcessed, sum.MessagesAdded, sum.Requests, sum.Partial)
				}
			}
		}
		if operation == "add" {
			errs = append(errs, runPostSourceCreateMigrationsForInvocation(st, state))
		} else {
			errs = append(errs, rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state))
		}
		if asJSON {
			errs = append(errs, json.MarshalWrite(cmd.OutOrStdout(), results))
			_, _ = fmt.Fprintln(cmd.OutOrStdout())
		}
		return errors.Join(errs...)
	}
	return command
}

func runConfiguredTikTokCommentsSync(ctx context.Context, st *store.Store, cfg *config.Config, profile config.TikTokSource) (retErr error) {
	identifier := tiktok.SourceIdentifier(profile.AppID, profile.AccountID)
	defer func() { _ = recordSyncAttempt(ctx, tiktok.CommentsSourceType, identifier, retErr) }()
	client, err := configuredTikTokClient(cfg, profile)
	if err != nil {
		return err
	}
	sum, err := tiktok.NewImporter(st, client).Import(ctx, profile.AppID, profile.AccountID, tiktok.ImportOptions{})
	if sum == nil {
		return err
	}
	job, _ := api.SchedulerJobNameForSource(tiktok.CommentsSourceType, identifier)
	return errors.Join(err, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), job))
}

func registerTikTokCommentJobs(sched *scheduler.Scheduler, state *invocation, st *store.Store, maintenance *attachmentMaintenance) {
	for _, profile := range state.cfg.ScheduledTikTokSources() {
		job, _ := api.SchedulerJobNameForSource(tiktok.CommentsSourceType, tiktok.SourceIdentifier(profile.AppID, profile.AccountID))
		err := sched.AddJob(scheduler.Job{Name: job, Schedule: profile.Schedule, Run: invocationBoundJobRun(state, func(ctx context.Context) error {
			return runScheduledSource(ctx, maintenance, false, func(ctx context.Context) error { return runConfiguredTikTokCommentsSync(ctx, st, state.cfg, profile) })
		})})
		if err != nil {
			state.logger.Error("failed to schedule TikTok comments", "source", profile.Identifier, "error", err)
		}
	}
}

func init() {
	var source *cobra.Command
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "source" {
			source = cmd
			break
		}
	}
	if source == nil {
		source = &cobra.Command{Use: "source", Short: "Add and sync provider sources"}
		rootCmd.AddCommand(source)
	}
	source.AddCommand(newTikTokCommentsCmd())
}
