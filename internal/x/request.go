package x

import (
	"context"
	"errors"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
)

var ErrReadRetryDelay = errors.New("X read retry would exceed fifteen minutes of waiting")

func xReadRetryDelay(apiError *APIError, attempt int, now time.Time) (time.Duration, error) {
	delay := time.Duration(0)
	known := false
	if apiError != nil {
		if retry, ok := httpretry.ParseRetryAfter(apiError.RetryAfter, 0, now); ok {
			delay = retry
			known = true
		}
		if reset, err := strconv.ParseInt(apiError.RateLimitReset, 10, 64); apiError.StatusCode == 429 && err == nil && reset >= 0 {
			delay = max(delay, max(time.Unix(reset, 0).Sub(now), 0))
			known = true
		}
	}
	if !known {
		delay = httpretry.RetryAfterAt("", attempt, httpretry.ProviderMaxRetryAfter, now)
	}
	if delay > 15*time.Minute {
		return 0, ErrReadRetryDelay
	}
	return delay, nil
}

func waitXRead(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
