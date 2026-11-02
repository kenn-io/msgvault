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
	"go.kenn.io/msgvault/internal/tiktokwebhook"
)

func selectedTikTokMessageProfiles(cfg *config.Config, args []string) ([]config.TikTokSource, error) {
	profiles, err := selectedTikTokProfiles(cfg, args)
	if err != nil {
		return nil, err
	}
	var messages []config.TikTokSource
	for _, profile := range profiles {
		if profile.MessagesEnabled {
			messages = append(messages, profile)
		}
	}
	if len(messages) == 0 {
		return nil, errors.New("no enabled TikTok messaging profiles\nhint: set messages_enabled = true on an enabled [[tiktok]] profile")
	}
	return messages, nil
}

func newTikTokMessagesCmd() *cobra.Command {
	group := &cobra.Command{Use: "tiktok-messages", Short: "Capture TikTok Business Messaging and inspect webhook delivery"}
	group.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if pre := cmd.Root().PersistentPreRunE; pre != nil {
			return tikTokCommandError(cmd, pre(cmd, args))
		}
		return nil
	}
	for _, operation := range []string{tikTokAddCommandName, "sync", statusValue, "replay"} {
		group.AddCommand(newTikTokMessagesAction(operation))
	}
	return group
}
func newTikTokMessagesAction(operation string) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{Use: operation + " [identifier]", Short: map[string]string{tikTokAddCommandName: "Register and backfill a configured messaging account", "sync": "Read the limited recent message window", statusValue: "Show receiver state and durable delivery counts", "replay": "Retry held and failed deliveries for the selected app"}[operation], Args: func(cmd *cobra.Command, args []string) error {
		return tikTokCommandError(cmd, cobra.MaximumNArgs(1)(cmd, args))
	}}
	command.SetFlagErrorFunc(tikTokCommandError)
	command.Flags().BoolVar(&asJSON, "json", false, "print JSON results; errors use JSON on stderr")
	if operation == tikTokAddCommandName || operation == "sync" {
		addManualSyncCacheFlags(command)
	}
	command.RunE = func(cmd *cobra.Command, args []string) (retErr error) {
		defer func() { retErr = tikTokCommandError(cmd, retErr) }()
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		if operation == statusValue || operation == "replay" {
			return runTikTokDeliveryControl(cmd, state.cfg, args, operation, asJSON)
		}
		profiles, err := selectedTikTokMessageProfiles(state.cfg, args)
		if err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		ctx, stop := withInterruptCancel(cmd, "\nInterrupted. TikTok deliveries remain recoverable...")
		defer stop()
		var results []tikTokResult
		var errs []error
		writes := false
		for _, profile := range profiles {
			if ctx.Err() != nil {
				errs = append(errs, ctx.Err())
				break
			}
			result, err := runTikTokMessageProfile(ctx, st, state.cfg, profile, operation == tikTokAddCommandName)
			results = append(results, result)
			if err != nil {
				errs = append(errs, fmt.Errorf("TikTok profile %s: %w", profile.Identifier, err))
			}
			if sum := result.Summary; sum != nil {
				writes = writes || sum.MessagesProcessed > 0
				if !asJSON {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "TikTok %s: %d messages processed (%d added); partial: %t\n", profile.Identifier, sum.MessagesProcessed, sum.MessagesAdded, sum.Partial)
				}
			}
		}
		if operation == tikTokAddCommandName {
			errs = append(errs, runPostSourceCreateMigrationsForInvocation(st, state))
		}
		if writes {
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
func runTikTokMessageProfile(ctx context.Context, st *store.Store, cfg *config.Config, profile config.TikTokSource, add bool) (result tikTokResult, retErr error) {
	result.Profile = profile.Identifier
	defer func() {
		_ = recordSyncAttempt(ctx, tiktok.MessagesSourceType, tiktok.SourceIdentifier(profile.AppID, profile.AccountID), retErr)
	}()
	client, err := configuredTikTokClient(cfg, profile)
	if err != nil {
		return result, err
	}
	defer client.Close()
	imp := tikTokMessageImporter(st, client, cfg, profile)
	if add {
		source, err := imp.Register(ctx, profile.AppID, profile.AccountID)
		if err != nil {
			return result, err
		}
		result.SourceID = source.ID
	}
	result.Summary, retErr = imp.Import(ctx, profile.AppID, profile.AccountID)
	return result, retErr
}

func runTikTokDeliveryControl(cmd *cobra.Command, cfg *config.Config, args []string, operation string, asJSON bool) error {
	app := ""
	if len(args) > 0 {
		profile := cfg.GetTikTokSource(args[0])
		if profile == nil {
			return errors.New("TikTok profile not found\nhint: use a configured [[tiktok]] identifier")
		}
		app = profile.AppID
	}
	root := tikTokSpoolRoot(cfg)
	var status tiktokwebhook.Status
	snapshot := tikTokReceiverSnapshot{Listener: "disabled", Reason: "not configured"}
	if cfg.TikTokWebhooks.Enabled() {
		snapshot.Reason = "receiver has not started"
	}
	var spool *tiktokwebhook.Spool
	if _, err := os.Stat(root); err == nil {
		_, maximum, _ := cfg.TikTokWebhooks.Effective()
		spool, err = tiktokwebhook.OpenSpool(root, maximum)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(root, "receiver.json"))
		if err == nil {
			if json.Unmarshal(data, &snapshot) != nil {
				return errors.New("invalid TikTok receiver status snapshot")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		status, err = spool.Status(cmd.Context(), app)
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if operation == "replay" {
		count := 0
		if spool != nil {
			var err error
			count, err = spool.Replay(cmd.Context(), app)
			if err != nil {
				return err
			}
		}
		if asJSON {
			return json.MarshalWrite(cmd.OutOrStdout(), map[string]int{"requeued": count})
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Requeued %d TikTok deliveries. The running receiver processes them on its next drain.\n", count)
		if err != nil {
			return fmt.Errorf("write TikTok replay status: %w", err)
		}
		return nil
	}
	result := struct {
		Receiver        tikTokReceiverSnapshot `json:"receiver"`
		Deliveries      tiktokwebhook.Status   `json:"deliveries"`
		HistoryComplete bool                   `json:"history_complete"`
	}{Receiver: snapshot, Deliveries: status}
	if asJSON {
		return json.MarshalWrite(cmd.OutOrStdout(), result)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "TikTok receiver: %s %s\nPending: %d; held: %d; failed: %d; done: %d; unsupported EU: %d\nOnly messages captured after connection can be complete; pull history is limited.\n", snapshot.Listener, snapshot.Reason, status.Pending, status.Held, status.Failed, status.Done, status.UnsupportedEU)
	if err != nil {
		return fmt.Errorf("write TikTok delivery status: %w", err)
	}
	return nil
}

func registerTikTokMessageJobs(sched *scheduler.Scheduler, state *invocation, st *store.Store, maintenance *attachmentMaintenance) {
	for _, profile := range state.cfg.ScheduledTikTokMessageSources() {
		job, _ := api.SchedulerJobNameForSource(tiktok.MessagesSourceType, tiktok.SourceIdentifier(profile.AppID, profile.AccountID))
		err := sched.AddJob(scheduler.Job{Name: job, Schedule: profile.Schedule, Run: invocationBoundJobRun(state, func(ctx context.Context) error {
			return runScheduledSource(ctx, maintenance, true, func(ctx context.Context) error {
				result, err := runTikTokMessageProfile(ctx, st, state.cfg, profile, false)
				if result.Summary != nil {
					err = errors.Join(err, rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), job))
				}
				return err
			})
		})})
		if err != nil {
			state.logger.Error("failed to schedule TikTok messages", "source", profile.Identifier, "error", err)
		}
	}
}

func init() {
	for _, source := range rootCmd.Commands() {
		if source.Name() == tikTokSourceCommandName {
			source.AddCommand(newTikTokMessagesCmd())
			return
		}
	}
	group := &cobra.Command{Use: tikTokSourceCommandName, Short: "Add and sync provider sources"}
	group.AddCommand(newTikTokMessagesCmd())
	rootCmd.AddCommand(group)
}
