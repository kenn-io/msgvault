package embed

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// retryError wraps a transient Voyage error. Callers use errors.As to detect it.
// retryAfter is an optional server-specified delay (from a 429
// Retry-After header). retryAfterSet=true means the header was
// successfully parsed and the duration is authoritative — including
// the "Retry-After: 0" case meaning retry immediately. When
// retryAfterSet=false the caller should use its default backoff.
type retryError struct {
	err           error
	retryAfter    time.Duration
	retryAfterSet bool
}

func (e *retryError) Error() string { return e.err.Error() }
func (e *retryError) Unwrap() error { return e.err }

// parseRetryAfter parses an HTTP Retry-After header (RFC 7231 §7.1.3),
// which may be either a non-negative delta-seconds integer or an
// HTTP-date. Returns (duration, true) when the header was
// successfully parsed — including "Retry-After: 0" which a server
// uses to ask for an immediate retry — and (0, false) when the
// header is missing or unparseable so the caller can fall back to
// its default backoff. A delta-seconds integer is clamped to one
// hour so a misbehaving server can't stall a worker indefinitely.
// HTTP-date values that have already passed return (0, true) so an
// expired hint still beats the default backoff (closest reasonable
// interpretation: "you may retry now").
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	const maxWait = time.Hour
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		d := time.Duration(secs) * time.Second
		if d > maxWait {
			return maxWait, true
		}
		return d, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return 0, true
		}
		if d > maxWait {
			return maxWait, true
		}
		return d, true
	}
	return 0, false
}
