package jobctx

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestErrorAfterYieldPreservesIndependentCauses(t *testing.T) {
	providerFailure := errors.New("provider unavailable")
	inputBudget := errors.New("input budget exhausted")
	cooperative, requestYield := WithPreemption(t.Context())
	requestYield()
	yielded, cancelYield := context.WithCancelCause(t.Context())
	defer cancelYield(nil)
	cancelYield(ErrYieldedToWaiter)
	budget, cancelBudget := context.WithCancelCause(t.Context())
	defer cancelBudget(nil)
	cancelBudget(ErrRunBudgetExceeded)
	deadline, cancelDeadline := context.WithDeadlineCause(t.Context(), time.Time{}, ErrRunBudgetExceeded)
	defer cancelDeadline()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		err     error
		signals []error
		kept    []error
		removed []error
	}{
		{name: "pure cooperative yield", ctx: cooperative, err: ErrYieldedToWaiter, removed: []error{ErrYieldedToWaiter}},
		{name: "wrapped joined failure", ctx: cooperative, err: fmt.Errorf("request failed: %w", errors.Join(ErrYieldedToWaiter, providerFailure)), kept: []error{providerFailure}, removed: []error{ErrYieldedToWaiter}},
		{name: "independent cancellation", ctx: cooperative, err: errors.Join(ErrYieldedToWaiter, context.Canceled, context.DeadlineExceeded), kept: []error{context.Canceled, context.DeadlineExceeded}, removed: []error{ErrYieldedToWaiter}},
		{name: "no scheduler request", ctx: t.Context(), err: errors.Join(ErrYieldedToWaiter, providerFailure), kept: []error{providerFailure}, removed: []error{ErrYieldedToWaiter}},
		{name: "yield cancellation keeps independent deadline", ctx: yielded, err: context.DeadlineExceeded, kept: []error{context.DeadlineExceeded}},
		{name: "run budget cancellation", ctx: budget, err: errors.Join(context.Canceled, providerFailure), kept: []error{providerFailure}, removed: []error{context.Canceled}},
		{name: "scheduler deadline", ctx: deadline, err: errors.Join(context.DeadlineExceeded, ErrRunBudgetExceeded, providerFailure), kept: []error{providerFailure}, removed: []error{context.DeadlineExceeded, ErrRunBudgetExceeded}},
		{name: "explicit input budget", ctx: t.Context(), err: errors.Join(inputBudget, providerFailure), signals: []error{inputBudget}, kept: []error{providerFailure}, removed: []error{inputBudget}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ErrorAfterYield(tc.ctx, tc.err, tc.signals...)
			for _, kept := range tc.kept {
				require.ErrorIs(t, err, kept)
			}
			for _, removed := range tc.removed {
				require.NotErrorIs(t, err, removed)
			}
			if len(tc.kept) == 0 {
				require.NoError(t, err)
			}
		})
	}
}
