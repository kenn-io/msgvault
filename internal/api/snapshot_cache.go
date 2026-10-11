package api

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// snapshotComputeTimeout bounds one background snapshot computation, which
// runs on the server's lifetime context rather than a request's.
const snapshotComputeTimeout = 2 * time.Minute

// errSnapshotWaitTimeout distinguishes a caller wait budget from a failed computation.
var errSnapshotWaitTimeout = fmt.Errorf("snapshot wait budget exceeded: %w", context.DeadlineExceeded)

// snapshotCache serves an expensive read with bounded latency. Each get
// starts (or joins) a fresh computation and waits up to a budget; when the
// computation is slower than that and an earlier result exists, the earlier
// result is returned as stale while the computation finishes in the
// background and replaces it. With no earlier result the caller waits.
type snapshotCache[T any] struct {
	// logger records refreshes that fail while an earlier value is served;
	// nil uses the default logger.
	logger *slog.Logger
	// freshFor, when positive, serves a value younger than it and read at
	// the caller's version as current without waiting, so a caller that
	// retries after a slow computation gets the result it started. Past half
	// its age a background refresh keeps the value warm. A value from another
	// version, or an older one, still serves as the stale fallback. Zero
	// waits on a fresh computation every time.
	freshFor time.Duration

	mu      sync.Mutex
	entries map[string]*snapshotEntry[T]
}

type snapshotEntry[T any] struct {
	value   T
	asOf    time.Time
	version string
	has     bool
	flight  *snapshotFlight[T]
}

type snapshotFlight[T any] struct {
	done    chan struct{}
	value   T
	asOf    time.Time
	version string
	err     error
}

func (c *snapshotCache[T]) get(
	reqCtx context.Context,
	lifetimeCtx context.Context,
	key string,
	wait time.Duration,
	compute func(context.Context) (T, error),
) (T, time.Time, bool, error) {
	return c.getVersion(reqCtx, lifetimeCtx, key, "", wait, compute)
}

// getVersion is get for a value that version identifies, such as a revision
// the computation depends on; only a value read at the same version is
// served as fresh.
func (c *snapshotCache[T]) getVersion(
	reqCtx context.Context,
	lifetimeCtx context.Context,
	key string,
	version string,
	wait time.Duration,
	compute func(context.Context) (T, error),
) (T, time.Time, bool, error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*snapshotEntry[T])
	}
	entry := c.entries[key]
	if entry == nil {
		entry = &snapshotEntry[T]{}
		c.entries[key] = entry
	}
	if c.freshFor > 0 && entry.has && entry.version == version {
		if age := time.Since(entry.asOf); age < c.freshFor {
			if age >= c.freshFor/2 && entry.flight == nil {
				c.startLocked(reqCtx, lifetimeCtx, key, version, entry, compute)
			}
			value, asOf := entry.value, entry.asOf
			c.mu.Unlock()
			return value, asOf, false, nil
		}
	}
	flight := entry.flight
	if flight == nil || flight.version != version {
		// A computation for an older version cannot answer this caller.
		flight = c.startLocked(reqCtx, lifetimeCtx, key, version, entry, compute)
	}
	hasPrevious, previous, previousAsOf := entry.has, entry.value, entry.asOf
	c.mu.Unlock()

	var zero T
	if !hasPrevious {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-flight.done:
			return flight.value, flight.asOf, false, flight.err
		case <-timer.C:
			if err := reqCtx.Err(); err != nil {
				return zero, time.Time{}, false, err
			}
			return zero, time.Time{}, false, errSnapshotWaitTimeout
		case <-reqCtx.Done():
			return zero, time.Time{}, false, reqCtx.Err()
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-flight.done:
		if flight.err == nil {
			return flight.value, flight.asOf, false, nil
		}
	case <-timer.C:
	case <-reqCtx.Done():
		return zero, time.Time{}, false, reqCtx.Err()
	}
	if err := reqCtx.Err(); err != nil {
		return zero, time.Time{}, false, err
	}
	return previous, previousAsOf, true, nil
}

// startLocked starts a computation for entry at version; c.mu must be held.
func (c *snapshotCache[T]) startLocked(
	reqCtx context.Context,
	lifetimeCtx context.Context,
	key string,
	version string,
	entry *snapshotEntry[T],
	compute func(context.Context) (T, error),
) *snapshotFlight[T] {
	flight := &snapshotFlight[T]{done: make(chan struct{}), version: version}
	entry.flight = flight
	// Correlate a shared refresh with the request that started it, while
	// keeping cancellation tied to the server's lifetime.
	computeCtx := store.WithRequestID(lifetimeCtx, store.RequestIDFromContext(reqCtx))
	go c.run(computeCtx, key, entry, flight, compute)
	return flight
}

func (c *snapshotCache[T]) run(
	lifetimeCtx context.Context,
	key string,
	entry *snapshotEntry[T],
	flight *snapshotFlight[T],
	compute func(context.Context) (T, error),
) {
	ctx, cancel := context.WithTimeout(lifetimeCtx, snapshotComputeTimeout)
	defer cancel()
	asOf := time.Now()
	value, err := compute(ctx)
	c.mu.Lock()
	// A computation that a newer version superseded must not replace its
	// result; it still fills an empty entry.
	if err == nil && (entry.flight == flight || !entry.has) {
		entry.value, entry.asOf, entry.version, entry.has = value, asOf, flight.version, true
	} else if entry.has && lifetimeCtx.Err() == nil {
		// Callers are being served the earlier value as stale; without this
		// log a persistent failure would stay invisible.
		logger := c.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("snapshot refresh failed; serving the previous value",
			"key", key, "as_of", entry.asOf, "error", err)
	}
	flight.value, flight.asOf, flight.err = value, asOf, err
	close(flight.done)
	if entry.flight == flight {
		entry.flight = nil
	}
	c.mu.Unlock()
}
