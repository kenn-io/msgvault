// Package httpretry contains small helpers shared by HTTP clients.
package httpretry

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultMaxRetryAfter caps exponential retry delays and is the safe
	// default for callers that do not provide a provider-specific cap.
	DefaultMaxRetryAfter = 60 * time.Second
	// ProviderMaxRetryAfter is the finite cap for providers that can return
	// meaningful throttling windows longer than one minute.
	ProviderMaxRetryAfter = 15 * time.Minute
)

const maxDuration = time.Duration(1<<63 - 1)

// RetryAfter returns a Retry-After header delay capped by maximum. When the
// header is absent or invalid, it returns an exponential fallback capped by
// DefaultMaxRetryAfter and maximum. A zero or negative maximum uses
// DefaultMaxRetryAfter. Invalid and overflowing header values use the fallback
// instead of being treated as a very large provider delay.
func RetryAfter(header string, attempt int, maximum time.Duration) time.Duration {
	return RetryAfterAt(header, attempt, maximum, time.Now())
}

// RetryAfterAt is RetryAfter with an injected clock. In addition to the
// numeric form, it accepts the HTTP-date form defined for Retry-After.
func RetryAfterAt(header string, attempt int, maximum time.Duration, now time.Time) time.Duration {
	if maximum <= 0 {
		maximum = DefaultMaxRetryAfter
	}
	if delay, ok := parsedRetryAfterAt(header, maximum, now); ok {
		return delay
	}

	return capDelay(exponentialBackoff(attempt), maximum)
}

// RetryAfterAtWithBase is RetryAfterAt with a caller-defined exponential
// fallback base. A malformed nonempty header uses that same fallback instead
// of silently switching to the shared one-second default.
func RetryAfterAtWithBase(
	header string, attempt int, base, maximum time.Duration, now time.Time,
) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if maximum <= 0 {
		maximum = DefaultMaxRetryAfter
	}
	if delay, ok := parsedRetryAfterAt(header, maximum, now); ok {
		return delay
	}
	return exponentialBackoffWithBase(attempt, base, maximum)
}

func parsedRetryAfterAt(header string, maximum time.Duration, now time.Time) (time.Duration, bool) {
	if header == "" {
		return 0, false
	}
	trimmed := strings.TrimSpace(header)
	secs, err := strconv.ParseUint(trimmed, 10, 64)
	if err == nil {
		if delay, ok := secondsToDuration(secs); ok {
			return capDelay(delay, maximum), true
		}
	}
	if retryAt, parseErr := http.ParseTime(trimmed); parseErr == nil {
		delay := max(retryAt.Sub(now), time.Duration(0))
		return capDelay(delay, maximum), true
	}
	return 0, false
}

func secondsToDuration(seconds uint64) (time.Duration, bool) {
	if seconds > uint64(maxDuration/time.Second) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func exponentialBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return time.Second
	}
	if attempt >= 6 {
		return DefaultMaxRetryAfter
	}
	return time.Duration(1<<uint(attempt)) * time.Second
}

func exponentialBackoffWithBase(attempt int, base, maximum time.Duration) time.Duration {
	delay := base
	for range max(attempt, 0) {
		if delay >= maximum || delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	return min(delay, maximum)
}

func capDelay(delay, maximum time.Duration) time.Duration {
	if maximum > 0 && delay > maximum {
		return maximum
	}
	return delay
}
