package embed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/msgvault/internal/operations"
)

const operationRecorderTimeout = 5 * time.Second

type operationPass struct {
	recorder operations.Recorder
	id       operations.StableID
	kind     operations.Kind
	log      *slog.Logger
}

func beginOperationPass(
	ctx context.Context,
	recorder operations.Recorder,
	kind operations.Kind,
	scope operations.PassScope,
	log *slog.Logger,
) (*operationPass, *operations.Run, error) {
	if log == nil {
		log = slog.Default()
	}
	spec := scope.InvocationSpec(kind)
	if err := spec.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%s operation pass scope: %w", kind, err)
	}
	if recorder == nil {
		return nil, nil, fmt.Errorf("begin %s operation pass: operation recorder is required", kind)
	}
	begun, err := recorder.Begin(ctx, spec)
	if err != nil {
		log.Error("operation recorder begin failed", "kind", kind, "error", err)
		return nil, nil, fmt.Errorf("begin %s operation pass: %w", kind, err)
	}
	switch begun.Disposition {
	case operations.BeginCreated:
		return &operationPass{recorder: recorder, id: begun.ID, kind: kind, log: log}, nil, nil
	case operations.BeginTerminal:
		if begun.Terminal == nil {
			return nil, nil, fmt.Errorf("begin %s operation pass returned terminal without outcome", kind)
		}
		return nil, begun.Terminal, nil
	case operations.BeginActive:
		return nil, nil, fmt.Errorf("begin %s operation pass found an active invocation", kind)
	default:
		return nil, nil, fmt.Errorf("begin %s operation pass returned invalid disposition %q", kind, begun.Disposition)
	}
}

func (p *operationPass) checkpoint(ctx context.Context, counters operations.InvocationCounters) {
	if p == nil {
		return
	}
	checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationRecorderTimeout)
	defer cancel()
	if err := p.recorder.Checkpoint(checkpointCtx, p.id, counters); err != nil {
		p.log.Error("operation recorder checkpoint failed", "kind", p.kind, "error", err)
	}
}

func (p *operationPass) finish(
	ctx context.Context,
	counters operations.InvocationCounters,
	runErr error,
) {
	if p == nil {
		return
	}
	publicError := operationPublicError(ctx, runErr)
	state, err := operations.DeriveInvocationState(p.kind, counters, publicError)
	if err != nil {
		p.log.Error("operation recorder finish state failed", "kind", p.kind, "error", err)
		return
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationRecorderTimeout)
	defer cancel()
	if err := p.recorder.Finish(finishCtx, p.id, counters, state, publicError); err != nil {
		p.log.Error("operation recorder finish failed", "kind", p.kind, "error", err)
	}
}

func operationPublicError(ctx context.Context, runErr error) *operations.PublicError {
	if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return operations.FixedPublicError(operations.PublicErrorInvocationCancelled)
	}
	if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return operations.FixedPublicError(operations.PublicErrorInvocationTimeout)
	}
	if runErr != nil {
		return operations.FixedPublicError(operations.PublicErrorInvocationUpstreamFailed)
	}
	return nil
}

func finalRunCounters(result RunResult) operations.InvocationCounters {
	succeeded := int64(result.Succeeded)
	failed := int64(result.Failed)
	attempted := int64(result.Claimed)
	if accounted := succeeded + failed; attempted < accounted {
		attempted = accounted
	} else if attempted > accounted {
		failed += attempted - accounted
	}
	return operations.InvocationCounters{
		Attempted: attempted, Succeeded: succeeded, Failed: failed,
		Truncated: int64(result.Truncated),
	}
}

func checkpointRunCounters(result RunResult) operations.InvocationCounters {
	succeeded := int64(result.Succeeded)
	failed := int64(result.Failed)
	attempted := max(int64(result.Claimed), succeeded+failed)
	return operations.InvocationCounters{
		Attempted: attempted, Succeeded: succeeded, Failed: failed,
		Truncated: int64(result.Truncated),
	}
}

type operationPassContextKey struct{}

func contextWithOperationPass(ctx context.Context, pass *operationPass) context.Context {
	if pass == nil {
		return ctx
	}
	return context.WithValue(ctx, operationPassContextKey{}, pass)
}

func checkpointContextOperationPass(ctx context.Context, result RunResult) {
	pass, _ := ctx.Value(operationPassContextKey{}).(*operationPass)
	pass.checkpoint(ctx, checkpointRunCounters(result))
}

func runResultFromOperationRun(run *operations.Run) (RunResult, error) {
	if run == nil {
		return RunResult{}, errors.New("operation pass terminal outcome is required")
	}
	counters, err := operations.InvocationCountersFromPublic(run.ID.Kind(), run.Counters)
	if err != nil {
		return RunResult{}, err
	}
	return RunResult{
		Claimed: int(counters.Attempted), Succeeded: int(counters.Succeeded),
		Failed: int(counters.Failed), Truncated: int(counters.Truncated),
	}, operations.TerminalReplayOutcome(run)
}

type messagePassOutcomes struct {
	pass      *operationPass
	attempted map[int64]struct{}
	succeeded map[int64]struct{}
	failed    map[int64]struct{}
	truncated int64
}

func newMessagePassOutcomes(pass *operationPass) *messagePassOutcomes {
	return &messagePassOutcomes{
		pass: pass, attempted: make(map[int64]struct{}),
		succeeded: make(map[int64]struct{}), failed: make(map[int64]struct{}),
	}
}

func (o *messagePassOutcomes) attempt(ids []int64) {
	for _, id := range ids {
		o.attempted[id] = struct{}{}
	}
}

func (o *messagePassOutcomes) succeed(ids []int64) {
	for _, id := range ids {
		o.attempted[id] = struct{}{}
		o.succeeded[id] = struct{}{}
		delete(o.failed, id)
	}
}

func (o *messagePassOutcomes) fail(ids []int64) {
	for _, id := range ids {
		o.attempted[id] = struct{}{}
		if _, ok := o.succeeded[id]; !ok {
			o.failed[id] = struct{}{}
		}
	}
}

func (o *messagePassOutcomes) addTruncated(count int) {
	o.truncated += int64(count)
}

func (o *messagePassOutcomes) counters(final bool) operations.InvocationCounters {
	failed := int64(len(o.failed))
	if final {
		accounted := len(o.succeeded) + len(o.failed)
		failed += int64(len(o.attempted) - accounted)
	}
	return operations.InvocationCounters{
		Attempted: int64(len(o.attempted)), Succeeded: int64(len(o.succeeded)),
		Failed: failed, Truncated: o.truncated,
	}
}

func (o *messagePassOutcomes) checkpoint(ctx context.Context) {
	o.pass.checkpoint(ctx, o.counters(false))
}

func idsWithout(ids, excluded []int64) []int64 {
	if len(excluded) == 0 {
		return append([]int64(nil), ids...)
	}
	blocked := make(map[int64]struct{}, len(excluded))
	for _, id := range excluded {
		blocked[id] = struct{}{}
	}
	kept := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := blocked[id]; !ok {
			kept = append(kept, id)
		}
	}
	return kept
}
