package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

var rebuildLineCacheAfterScheduledSync = rebuildCacheAfterScheduledSync

// Track this pass independently, then carry its progress into the scheduler's
// context so cancellation after a durable write still permits a follow-up.
func runLineProcessor(ctx context.Context, channelID string, process func(context.Context) error) error {
	progress := jobctx.WithProgress(ctx)
	err := process(progress)
	if !jobctx.HasProgress(progress) {
		return err
	}
	jobctx.RecordProgress(ctx)
	name, _ := api.SchedulerJobNameForSource(line.SourceType, channelID)
	return errors.Join(err, rebuildLineCacheAfterScheduledSync(context.WithoutCancel(ctx), name))
}

type lineRuntime struct {
	receiver  *line.Receiver
	scheduler *scheduler.Scheduler
	jobs      []string
	logger    *slog.Logger
}

// Construct only from local registration and daemon environment. No provider
// request belongs in serve initialization; incomplete channels can be added
// through the running daemon without preventing healthy channels from starting.
func newLineRuntime(ctx context.Context, st *store.Store, state *invocation, sched *scheduler.Scheduler, maintenance *attachmentMaintenance, logger *slog.Logger) (*lineRuntime, error) {
	if state == nil || state.cfg == nil {
		return nil, errors.New("LINE configuration is unavailable")
	}
	runtime := &lineRuntime{scheduler: sched, logger: repairLogger(logger)}
	cfg := state.cfg
	var channels []line.ReceiverChannel
	for _, profile := range cfg.Line {
		if !profile.Enabled {
			continue
		}
		source, err := st.GetSourceByTypeAndIdentifier(line.SourceType, profile.ChannelID)
		if err == nil && source.MergedIntoSourceID != 0 {
			err = store.ErrSourceRetired
		}
		if err != nil {
			runtime.logger.Warn("LINE receiver skipped; register an active source with add-line", "channel", profile.ChannelID, "error", err)
			continue
		}
		client, err := configuredLineClient(profile)
		if err != nil {
			runtime.logger.Warn("LINE receiver skipped; set secret in daemon environment and restart", "channel", profile.ChannelID, "secret_env", profile.SecretEnv)
			continue
		}
		botID, err := st.LineBotUserIDContext(ctx, source.ID)
		if err != nil || botID == "" {
			runtime.logger.Warn("LINE receiver skipped; run add-line to register bot identity", "channel", profile.ChannelID)
			continue
		}
		processor := line.NewProcessor(st, client, source.ID, profile.ChannelID, cfg.AttachmentsDir())
		channelID := profile.ChannelID
		name, _ := api.SchedulerJobNameForSource(line.SourceType, channelID)
		run := invocationBoundJobRun(state, func(ctx context.Context) error {
			return runScheduledSource(ctx, maintenance, true, func(ctx context.Context) error {
				err := runLineProcessor(ctx, channelID, processor.Process)
				_ = recordSyncAttempt(ctx, line.SourceType, channelID, err)
				return err
			})
		})
		if err := sched.AddJob(scheduler.Job{Name: name, Interval: 30 * time.Second, Preemptible: true, Background: true, Run: run}); err != nil {
			return nil, fmt.Errorf("schedule LINE inbox: %w", err)
		}
		runtime.jobs = append(runtime.jobs, name)
		channels = append(channels, line.ReceiverChannel{ChannelID: channelID, SourceID: source.ID, BotUserID: botID, SecretEnv: profile.SecretEnv, Secret: os.Getenv(profile.SecretEnv)})
	}
	receiver, err := line.NewReceiver(st, channels, runtime.wake)
	if err != nil {
		return nil, err
	}
	runtime.receiver = receiver
	return runtime, nil
}

func (r *lineRuntime) wake(channelID string) {
	name, _ := api.SchedulerJobNameForSource(line.SourceType, channelID)
	if _, err := r.scheduler.StartJob(name, false); err != nil {
		r.logger.Warn("LINE inbox wake could not be queued", "channel", channelID, "error", err)
	}
}

func (r *lineRuntime) queueStartup() {
	for _, name := range r.jobs {
		if _, err := r.scheduler.StartJob(name, false); err != nil {
			r.logger.Warn("LINE startup processing could not be queued", "job", name, "error", err)
		}
	}
}
