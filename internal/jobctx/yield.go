package jobctx

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrYieldedToWaiter is the cancellation cause used when a resumable job
// steps aside so a waiting operation can acquire the work gate.
var ErrYieldedToWaiter = errors.New("yielded to a waiting operation")

// YieldedToWaiter reports whether ctx was cancelled because a job yielded to
// a waiting operation.
func YieldedToWaiter(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrYieldedToWaiter)
}

// ErrorAfterYield removes scheduler cancellation from err while preserving
// independent failures, including failures joined with the cancellation.
func ErrorAfterYield(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if YieldedToWaiter(ctx) || errors.Is(context.Cause(ctx), ErrRunBudgetExceeded) {
		_, filtered := filterYieldCancellation(err, errors.Is(ctx.Err(), context.DeadlineExceeded))
		return filtered
	}
	return err
}

type yieldFilteredError struct {
	message string
	cause   error
}

func (e yieldFilteredError) Error() string { return e.message }

func (e yieldFilteredError) Unwrap() error { return e.cause }

func filterYieldCancellation(err error, deadlineExpired bool) (bool, error) {
	if err == nil {
		return false, nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		kept := make([]error, 0, len(causes))
		changed := false
		for _, cause := range causes {
			filteredOut, filtered := filterYieldCancellation(cause, deadlineExpired)
			changed = changed || filteredOut
			if filtered != nil {
				kept = append(kept, filtered)
			}
		}
		if !changed {
			return false, err
		}
		return true, errors.Join(kept...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		cause := wrapped.Unwrap()
		changed, filtered := filterYieldCancellation(cause, deadlineExpired)
		if !changed {
			return false, err
		}
		if filtered == nil {
			return true, nil
		}
		message := err.Error()
		if causeMessage := cause.Error(); causeMessage != "" {
			message = strings.Replace(message, causeMessage, filtered.Error(), 1)
		}
		if message == err.Error() {
			message = fmt.Sprintf("%s: %s", message, filtered.Error())
		}
		return true, yieldFilteredError{message: message, cause: filtered}
	}
	if errors.Is(err, context.Canceled) || (deadlineExpired && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRunBudgetExceeded))) || errors.Is(err, ErrYieldedToWaiter) {
		return true, nil
	}
	return false, err
}
