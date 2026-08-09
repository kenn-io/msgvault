// Package httpretry contains small helpers shared by HTTP clients.
package httpretry

import (
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
	if maximum <= 0 {
		maximum = DefaultMaxRetryAfter
	}
	if header != "" {
		secs, err := strconv.ParseUint(strings.TrimSpace(header), 10, 64)
		if err == nil {
			if delay, ok := secondsToDuration(secs); ok {
				return capDelay(delay, maximum)
			}
		}
	}

	return capDelay(exponentialBackoff(attempt), maximum)
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

func capDelay(delay, maximum time.Duration) time.Duration {
	if maximum > 0 && delay > maximum {
		return maximum
	}
	return delay
}
