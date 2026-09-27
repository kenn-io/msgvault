package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// snapshotComputeTimeout bounds one background snapshot computation, which
// runs on the server's lifetime context rather than a request's.
const snapshotComputeTimeout = 2 * time.Minute

// snapshotCache serves an expensive read with bounded latency. Each get
// starts (or joins) a fresh computation and waits up to a budget; when the
// computation is slower than that and an earlier result exists, the earlier
// result is returned as stale while the computation finishes in the
// background and replaces it. With no earlier result the caller waits.
type snapshotCache[T any] struct {
	// logger records refreshes that fail while an earlier value is served;
	// nil uses the default logger.
	logger *slog.Logger

	mu      sync.Mutex
	entries map[string]*snapshotEntry[T]
}

type snapshotEntry[T any] struct {
	value  T
	asOf   time.Time
	has    bool
	flight *snapshotFlight[T]
}

type snapshotFlight[T any] struct {
	done  chan struct{}
	value T
	asOf  time.Time
	err   error
}

func (c *snapshotCache[T]) get(
	reqCtx context.Context,
	lifetimeCtx context.Context,
	key string,
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
	flight := entry.flight
	if flight == nil {
		flight = &snapshotFlight[T]{done: make(chan struct{})}
		entry.flight = flight
		// Correlate a shared refresh with the request that started it, while
		// keeping cancellation tied to the server's lifetime.
		computeCtx := store.WithRequestID(lifetimeCtx, store.RequestIDFromContext(reqCtx))
		go c.run(computeCtx, key, entry, flight, compute)
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
			return zero, time.Time{}, false, context.DeadlineExceeded
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
	if err == nil {
		entry.value, entry.asOf, entry.has = value, asOf, true
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
	entry.flight = nil
	c.mu.Unlock()
}
