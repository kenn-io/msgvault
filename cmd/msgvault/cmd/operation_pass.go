package cmd

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/operations"
)

const commandOperationRecorderTimeout = 5 * time.Second

type commandOperationPass struct {
	recorder operations.Recorder
	id       operations.StableID
	kind     operations.Kind
}

func newOperationPassScope(prefix string, trigger operations.Trigger) operations.PassScope {
	return operations.PassScope{
		Key: prefix + ":" + uuid.NewString(), Trigger: trigger, StartedAt: time.Now().UTC(),
	}
}

func beginCommandOperationPass(
	ctx context.Context, recorder operations.Recorder, kind operations.Kind, scope operations.PassScope,
) (*commandOperationPass, *operations.Run, error) {
	spec := scope.InvocationSpec(kind)
	if err := spec.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%s operation pass scope: %w", kind, err)
	}
	if operationRecorderIsNil(recorder) {
		return nil, nil, fmt.Errorf("begin %s operation pass: operation recorder is required", kind)
	}
	begun, err := recorder.Begin(ctx, spec)
	if err != nil {
		return nil, nil, fmt.Errorf("begin %s operation pass: %w", kind, err)
	}
	switch begun.Disposition {
	case operations.BeginCreated:
		return &commandOperationPass{recorder: recorder, id: begun.ID, kind: kind}, nil, nil
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

func operationRecorderIsNil(recorder operations.Recorder) bool {
	if recorder == nil {
		return true
	}
	value := reflect.ValueOf(recorder)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (p *commandOperationPass) checkpoint(ctx context.Context, counters operations.InvocationCounters) {
	if p == nil {
		return
	}
	checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandOperationRecorderTimeout)
	defer cancel()
	if err := p.recorder.Checkpoint(checkpointCtx, p.id, counters); err != nil {
		if state := invocationFromContext(ctx); state != nil && state.logger != nil {
			state.logger.Error("operation recorder checkpoint failed", "kind", p.kind, "error", err)
		}
	}
}

func (p *commandOperationPass) finish(
	ctx context.Context, counters operations.InvocationCounters, runErr error,
) {
	if p == nil {
		return
	}
	publicError := commandOperationPublicError(ctx, runErr)
	state, err := operations.DeriveInvocationState(p.kind, counters, publicError)
	if err != nil {
		if state := invocationFromContext(ctx); state != nil && state.logger != nil {
			state.logger.Error("operation recorder finish state failed", "kind", p.kind, "error", err)
		}
		return
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandOperationRecorderTimeout)
	defer cancel()
	if err := p.recorder.Finish(finishCtx, p.id, counters, state, publicError); err != nil {
		if inv := invocationFromContext(ctx); inv != nil && inv.logger != nil {
			inv.logger.Error("operation recorder finish failed", "kind", p.kind, "error", err)
		}
	}
}

func commandOperationPublicError(ctx context.Context, runErr error) *operations.PublicError {
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
