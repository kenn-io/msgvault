package personenrichment

import (
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
)

type RetryInput struct {
	Attempt    int
	Base       time.Duration
	Maximum    time.Duration
	RetryAfter string
	Now        time.Time
	Jitter     func(time.Duration) time.Duration
}

// RetryDelay calculates a bounded durable retry delay. The caller persists
// the resulting next-action time; workers never sleep while holding a lease.
func RetryDelay(input RetryInput) (time.Duration, error) {
	if input.Attempt < 0 {
		return 0, errors.New("retry attempt must be non-negative")
	}
	if input.Base <= 0 || input.Maximum <= 0 {
		return 0, errors.New("retry base and maximum must be positive")
	}
	if input.Now.IsZero() {
		return 0, errors.New("retry clock is required")
	}
	if input.Jitter == nil {
		return 0, errors.New("retry jitter function is required")
	}

	delay := httpretry.RetryAfterAtWithBase(
		input.RetryAfter, input.Attempt, input.Base, input.Maximum, input.Now)
	jitter := input.Jitter(delay)
	if jitter < 0 {
		return 0, errors.New("retry jitter must be non-negative")
	}
	if delay >= input.Maximum || jitter > input.Maximum-delay {
		return input.Maximum, nil
	}
	return delay + jitter, nil
}
