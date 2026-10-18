package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const peopleTierJobName = "people-tiers"
const peopleTierJobMaxRuntime = 2 * time.Minute

type peopleTierScheduledRunner struct {
	store     *store.Store
	config    persontiers.Config
	timezone  string
	maxDirect int
	gate      api.LabeledOperationGate
	now       func() time.Time
	logger    *slog.Logger
}

func registerPeopleTierJob(sched *scheduler.Scheduler, st *store.Store, cfg *config.Config, gate api.LabeledOperationGate, logger *slog.Logger) error {
	if !cfg.People.Tiers.Enabled {
		return nil
	}
	if err := cfg.People.Tiers.Validate(); err != nil {
		return err
	}
	if logger == nil {
		logger = slog.Default()
	}
	runner := peopleTierScheduledRunner{
		store: st, config: cfg.People.Tiers, timezone: cfg.Activity.Timezone,
		maxDirect: cfg.Activity.MaxDirectCounterparts, gate: gate,
		now: time.Now, logger: logger,
	}
	return sched.AddJob(newPeopleTierScheduledJob(runner))
}

func newPeopleTierScheduledJob(r peopleTierScheduledRunner) scheduler.Job {
	return scheduler.Job{
		Name: peopleTierJobName, Schedule: r.config.Schedule, Run: r.Run,
		// This job waits for the daemon gate inside Run. Its work budget starts
		// after admission so time queued behind an active operation cannot
		// consume the bounded pass before evaluation begins.
		Preemptible: true,
	}
}

// Run commits one page and releases admission before requesting another pass.
// The surrounding scheduler must not also hold the operation gate.
func (r peopleTierScheduledRunner) Run(ctx context.Context) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	release, ok := r.gate.BeginLabeledWorkContext(ctx, "People tier evaluation")
	if !ok {
		if err := ctx.Err(); err != nil {
			return err
		}
		return scheduler.ErrDeferUntilNextTrigger
	}
	defer release()
	budgetCtx, stopBudget := context.WithTimeoutCause(ctx, peopleTierJobMaxRuntime, jobctx.ErrRunBudgetExceeded)
	defer stopBudget()
	pageCtx, stopYield := personTierYieldContext(budgetCtx, r.gate)
	defer stopYield()
	finished := false
	defer func() {
		if finished {
			return
		}
		resultErr = personTierScheduledRunResult(pageCtx, ctx, resultErr)
	}()

	// Reading first preserves a superseded marker left by reset or manual
	// apply. GetOrStart would immediately replace that marker with a new run.
	run, err := r.store.PersonTierRunContext(pageCtx, "scheduled")
	if err != nil {
		return fmt.Errorf("read scheduled people tier run: %w", err)
	}
	if run == nil {
		mode := "record_only"
		if r.config.Apply {
			mode = "apply"
		}
		run, err = r.store.GetOrStartPersonTierRunContext(pageCtx, store.PersonTierPageRequest{
			Config: r.config, Timezone: r.timezone, MaxDirect: r.maxDirect, AsOf: r.now(),
		}, "scheduled", mode)
		if err != nil {
			return fmt.Errorf("start scheduled people tier run: %w", err)
		}
	}
	page, err := r.store.ProcessPersonTierRunPageWithCheckpointContext(pageCtx, *run, 500, peopleTierJobName, func() {
		jobctx.RecordProgress(ctx)
	})
	if err == store.ErrPersonTierRunSuperseded { //nolint:errorlint // A joined independent error must remain visible.
		r.logger.InfoContext(ctx, "scheduled people tier run superseded; waiting for next trigger", "run_id", run.RunID)
		return scheduler.ErrDeferUntilNextTrigger
	}
	if err != nil {
		return fmt.Errorf("process scheduled people tier page: %w", err)
	}
	jobctx.RecordProgress(ctx)
	r.logger.InfoContext(ctx, "scheduled people tier page committed", "run_id", run.RunID,
		"mode", run.Mode, "counts", page.Counts, "remaining", page.NextCursor != "")
	if page.NextCursor != "" {
		return scheduler.ErrReschedule
	}
	// The final commit cleared the run. A waiter arriving now must not ask
	// the scheduler to begin a fresh scan. Pruning stays within this pass budget.
	finished = true
	stopYield()
	if err := r.store.PrunePersonTierPreviewsContext(budgetCtx, r.config.PreviewRetentionDays); err != nil {
		return &scheduler.DeferError{Err: fmt.Errorf("prune people tier previews: %w", err)}
	}
	return scheduler.ErrDeferUntilNextTrigger
}

func personTierScheduledRunResult(pageCtx, runCtx context.Context, resultErr error) error {
	budgetExpired := errors.Is(context.Cause(pageCtx), jobctx.ErrRunBudgetExceeded)
	if !jobctx.YieldedToWaiter(pageCtx) && !budgetExpired {
		return resultErr
	}
	if resultErr == scheduler.ErrDeferUntilNextTrigger || resultErr == scheduler.ErrReschedule { //nolint:errorlint // Preserve the scheduler's bare control signals without hiding joined failures.
		return resultErr
	}
	if budgetExpired {
		if err := jobctx.ErrorAfterYield(pageCtx, resultErr); err != nil {
			return &scheduler.RescheduleError{Err: err}
		}
		if jobctx.HasProgress(runCtx) {
			return scheduler.ErrReschedule
		}
		return fmt.Errorf("%w before committing progress", jobctx.ErrRunBudgetExceeded)
	}
	if err := jobctx.ErrorAfterYield(pageCtx, resultErr); err != nil {
		return &scheduler.RescheduleError{Err: err}
	}
	return scheduler.ErrReschedule
}

// personTierYieldContext supplies waiter cancellation for bounded jobs whose
// scheduler does not hold the operation gate around the whole callback.
func personTierYieldContext(parent context.Context, gate api.LabeledOperationGate) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	if gate.HasRequestWaiters() {
		cancel(jobctx.ErrYieldedToWaiter)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if gate.HasRequestWaiters() {
					cancel(jobctx.ErrYieldedToWaiter)
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(nil); <-stopped }
}
