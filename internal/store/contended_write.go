package store

import (
	"context"
	"fmt"
	"math/rand"
	"time"
)

const (
	maxContendedWriteAttempts = 12

	// contendedWriteBackoffBase is the ceiling of the first retry delay; each
	// further attempt doubles it up to contendedWriteBackoffMax. The twelve
	// attempts therefore sleep at most 1278ms in total, and half that on
	// average once the jitter is applied. Nothing sleeps unless the database
	// is genuinely contended: the first attempt is never delayed, and most
	// contended writes get through on the one after it.
	//
	// The budget has to cover the longest a competing writer can keep the
	// writer lock away from us, which on an oversubscribed host is set by
	// scheduling delay rather than by the size of the transaction — a writer
	// descheduled between its reads and its write blocks the loser for far
	// longer than the write itself takes, and for the same reason the early,
	// short sleeps are swamped by scheduler latency and buy less spread than
	// their nominal length suggests.
	contendedWriteBackoffBase = 2 * time.Millisecond
	contendedWriteBackoffMax  = 256 * time.Millisecond
)

// retryContendedWrite retries read-then-write transactions that can fail
// snapshot upgrade (SQLITE_BUSY), deadlock, or race a unique index. Every
// store writer that can lose one of those races uses this one bounded policy.
//
// The retries sleep, because the snapshot-upgrade failure gets no help from
// _busy_timeout. These transactions are deferred: they read first and only
// then write, so by the time one asks for the WAL writer lock it is already
// holding a read snapshot. SQLite will not run the busy handler when waiting
// could deadlock, so it returns SQLITE_BUSY to the caller immediately — the
// losing transaction has to roll back and start over from its reads. Retrying
// with no delay simply re-collides with the writer that is still committing,
// and every loser wakes in lockstep, so under load several writers can burn
// the whole attempt budget without any of them getting through. Exponential
// backoff with full jitter spreads them apart instead.
//
// A cancelled ctx ends the loop at the next backoff with ctx.Err(); an attempt
// already running sees the cancellation through its own statements.
func retryContendedWrite[T any](
	ctx context.Context, s *Store, operation string, attempt func() (*T, error),
) (*T, error) {
	var lastErr error
	for i := range maxContendedWriteAttempts {
		write, err := attempt()
		if err == nil {
			return write, nil
		}
		if !s.dialect.IsConflictError(err) && !s.dialect.IsBusyError(err) {
			return nil, err
		}
		lastErr = err
		if i == maxContendedWriteAttempts-1 {
			break
		}
		select {
		case <-time.After(contendedWriteBackoff(i)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("%s: gave up after %d attempts: %w",
		operation, maxContendedWriteAttempts, lastErr)
}

// retryBusyWrite retries only lock and transaction contention. Callers whose
// unique constraints describe deterministic domain conflicts use this form so
// they return the first typed failure instead of repeating an expensive write.
func retryBusyWrite[T any](
	ctx context.Context, s *Store, operation string, attempt func() (*T, error),
) (*T, error) {
	var lastErr error
	for i := range maxContendedWriteAttempts {
		write, err := attempt()
		if err == nil {
			return write, nil
		}
		if !s.dialect.IsBusyError(err) {
			return nil, err
		}
		lastErr = err
		if i == maxContendedWriteAttempts-1 {
			break
		}
		select {
		case <-time.After(contendedWriteBackoff(i)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("%s: gave up after %d attempts: %w",
		operation, maxContendedWriteAttempts, lastErr)
}

// retryContendedWriteErr is retryContendedWrite for writers that return no
// value.
func retryContendedWriteErr(
	ctx context.Context, s *Store, operation string, attempt func() error,
) error {
	_, err := retryContendedWrite(ctx, s, operation, func() (*struct{}, error) {
		return nil, attempt()
	})
	return err
}

// retryBusyWriteErr is retryBusyWrite for writers that return no value.
func retryBusyWriteErr(
	ctx context.Context, s *Store, operation string, attempt func() error,
) error {
	_, err := retryBusyWrite(ctx, s, operation, func() (*struct{}, error) {
		return nil, attempt()
	})
	return err
}

// contendedWriteBackoff returns the delay before retrying the attempt after
// the given zero-based one, growing exponentially to a cap. math/rand is fine
// here — full jitter only has to spread competing writers apart, it is not
// security-sensitive.
func contendedWriteBackoff(attempt int) time.Duration {
	ceiling := contendedWriteBackoffBase << attempt
	if ceiling > contendedWriteBackoffMax || ceiling <= 0 {
		ceiling = contendedWriteBackoffMax
	}
	return time.Duration(rand.Int63n(int64(ceiling))) //nolint:gosec // not security-sensitive
}
