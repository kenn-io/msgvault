package embed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/msgvault/internal/operations"
)

type testOperationInvocation struct {
	spec     operations.InvocationSpec
	id       operations.StableID
	counters operations.InvocationCounters
	state    operations.State
	run      *operations.Run
}

type testOperationContext struct {
	err         error
	hasDeadline bool
}

// testOperationRecorder is an in-memory lifecycle implementation, not a
// call-count mock. It validates the same begin/checkpoint/finish invariants the
// durable recorder exposes while allowing individual persistence seams to fail.
type testOperationRecorder struct {
	mu                 sync.Mutex
	next               int64
	invocations        []*testOperationInvocation
	checkpointContexts []testOperationContext
	beginErr           error
	checkpointErr      error
	finishErr          error
}

func newTestOperationRecorder() *testOperationRecorder {
	return &testOperationRecorder{next: 1}
}

func (r *testOperationRecorder) Begin(
	_ context.Context, spec operations.InvocationSpec,
) (operations.BeginResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.beginErr != nil {
		return operations.BeginResult{}, r.beginErr
	}
	if err := spec.Validate(); err != nil {
		return operations.BeginResult{}, err
	}
	spec = spec.Normalized()
	for _, invocation := range r.invocations {
		if invocation.spec.Kind != spec.Kind || invocation.spec.Key != spec.Key {
			continue
		}
		if invocation.run != nil {
			run := *invocation.run
			return operations.BeginResult{ID: invocation.id, Disposition: operations.BeginTerminal, Terminal: &run}, nil
		}
		return operations.BeginResult{ID: invocation.id, Disposition: operations.BeginActive}, nil
	}
	id, err := operations.NewInt64ID(spec.Kind, r.next)
	if err != nil {
		return operations.BeginResult{}, err
	}
	r.next++
	r.invocations = append(r.invocations, &testOperationInvocation{spec: spec, id: id, state: operations.StateRunning})
	return operations.BeginResult{ID: id, Disposition: operations.BeginCreated}, nil
}

func (r *testOperationRecorder) Checkpoint(
	ctx context.Context, id operations.StableID, counters operations.InvocationCounters,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, hasDeadline := ctx.Deadline()
	r.checkpointContexts = append(r.checkpointContexts, testOperationContext{err: ctx.Err(), hasDeadline: hasDeadline})
	invocation, err := r.find(id)
	if err != nil {
		return err
	}
	if err := counters.ValidateCheckpoint(id.Kind(), invocation.counters); err != nil {
		return err
	}
	if invocation.state != operations.StateRunning {
		return errors.New("checkpoint requires a running invocation")
	}
	if r.checkpointErr != nil {
		return r.checkpointErr
	}
	invocation.counters = counters
	return nil
}

func (r *testOperationRecorder) Finish(
	_ context.Context,
	id operations.StableID,
	counters operations.InvocationCounters,
	state operations.State,
	publicError *operations.PublicError,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	invocation, err := r.find(id)
	if err != nil {
		return err
	}
	if invocation.state != operations.StateRunning {
		return errors.New("finish requires a running invocation")
	}
	if err := counters.ValidateCheckpoint(id.Kind(), invocation.counters); err != nil {
		return err
	}
	wantState, err := operations.DeriveInvocationState(id.Kind(), counters, publicError)
	if err != nil {
		return err
	}
	if state != wantState {
		return fmt.Errorf("finish state %q does not match derived state %q", state, wantState)
	}
	if r.finishErr != nil {
		return r.finishErr
	}
	finishedAt := time.Now().UTC()
	if finishedAt.Before(invocation.spec.StartedAt) {
		finishedAt = invocation.spec.StartedAt
	}
	trigger := invocation.spec.Trigger
	lane := operations.LaneMessages
	if id.Kind() == operations.KindPersonEmbedding {
		lane = operations.LanePersonFacts
	}
	run := operations.Run{
		ID: id, Lane: lane, State: state, Trigger: &trigger,
		StartedAt: invocation.spec.StartedAt, FinishedAt: &finishedAt,
		Counters: counters.PublicCounters(id.Kind()), Error: publicError,
	}
	if err := run.Validate(); err != nil {
		return err
	}
	invocation.counters = counters
	invocation.state = state
	invocation.run = &run
	return nil
}

func (r *testOperationRecorder) find(id operations.StableID) (*testOperationInvocation, error) {
	for _, invocation := range r.invocations {
		if invocation.id == id {
			return invocation, nil
		}
	}
	return nil, errors.New("operation invocation not found")
}

func (r *testOperationRecorder) invocation(key string) (testOperationInvocation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, invocation := range r.invocations {
		if invocation.spec.Key == key {
			cloned := *invocation
			return cloned, true
		}
	}
	return testOperationInvocation{}, false
}

func (r *testOperationRecorder) contexts() []testOperationContext {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]testOperationContext(nil), r.checkpointContexts...)
}

var testEmbeddingPassSequence atomic.Uint64

func testEmbeddingPassScope() operations.PassScope {
	sequence := testEmbeddingPassSequence.Add(1)
	return operations.PassScope{
		Key: fmt.Sprintf("test:embedding:%d", sequence), Trigger: operations.TriggerManual,
		StartedAt: time.Date(2026, 8, 30, 0, 0, 0, int(sequence), time.UTC),
	}
}
