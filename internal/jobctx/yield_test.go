package jobctx

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrorAfterYieldRemovesReturnedSignalsWithoutCancellation(t *testing.T) {
	providerFailure := errors.New("provider unavailable")
	inputBudget := errors.New("input budget exhausted")
	for _, tc := range []struct {
		name    string
		err     error
		signals []error
		kept    []error
	}{
		{name: "returned yield", err: ErrYieldedToWaiter},
		{name: "wrapped joined failure", err: fmt.Errorf("request failed: %w", errors.Join(ErrYieldedToWaiter, providerFailure)), kept: []error{providerFailure}},
		{name: "cancellation without a scheduler stop", err: errors.Join(ErrYieldedToWaiter, context.Canceled, context.DeadlineExceeded), kept: []error{context.Canceled, context.DeadlineExceeded}},
		{name: "explicit signal", err: errors.Join(inputBudget, providerFailure), signals: []error{inputBudget}, kept: []error{providerFailure}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ErrorAfterYield(t.Context(), tc.err, tc.signals...)
			if len(tc.kept) == 0 {
				require.NoError(t, err)
				return
			}
			for _, kept := range tc.kept {
				require.ErrorIs(t, err, kept)
			}
			for _, removed := range append(tc.signals, ErrYieldedToWaiter) {
				require.NotErrorIs(t, err, removed)
			}
		})
	}
}
