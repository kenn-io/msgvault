package embed

import "time"

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
