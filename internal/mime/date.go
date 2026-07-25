package mime

import (
	"slices"
	"strings"
	"time"
)

var plausibleDateLowerBound = time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)

const plausibleDateFutureSlack = 30 * 24 * time.Hour

// DateSource identifies the input selected as a message's canonical sent time.
type DateSource string

const (
	DateSourceHeader   DateSource = "date-header"
	DateSourceReceived DateSource = "received-oldest-plausible"
	DateSourceFallback DateSource = "source-fallback"
	DateSourceNone     DateSource = "none"
)

// IsPlausibleDate reports whether t falls inside the accepted range for an
// email send time. The small amount of future slack permits clock skew and
// scheduled messages without accepting timestamps decades in the future.
func IsPlausibleDate(t, now time.Time) bool {
	if t.IsZero() {
		return false
	}
	utc := t.UTC()
	return !utc.Before(plausibleDateLowerBound) &&
		!utc.After(now.UTC().Add(plausibleDateFutureSlack))
}

// PlausibleDateBounds returns the inclusive date range used by the resolver.
func PlausibleDateBounds(now time.Time) (time.Time, time.Time) {
	return plausibleDateLowerBound, now.UTC().Add(plausibleDateFutureSlack)
}

// ParseReceivedChain extracts the timestamp following the final semicolon in
// each Received field. The returned slice preserves source order, which is
// normally newest hop first because receiving MTAs prepend trace fields.
func ParseReceivedChain(headers []string) []time.Time {
	dates := make([]time.Time, 0, len(headers))
	for _, header := range headers {
		semicolon := strings.LastIndex(header, ";")
		if semicolon < 0 {
			continue
		}
		if parsed := parseDate(strings.TrimSpace(header[semicolon+1:])); !parsed.IsZero() {
			dates = append(dates, parsed)
		}
	}
	return dates
}

// ResolveMessageDate selects a canonical sent time from the Date header, the
// Received trace, and provider metadata, in that order. Received fields are
// walked from the oldest hop toward the newest so the first plausible MTA
// timestamp wins.
func ResolveMessageDate(
	headerDate time.Time,
	receivedDates []time.Time,
	fallbackDate time.Time,
	now time.Time,
) (time.Time, DateSource) {
	if IsPlausibleDate(headerDate, now) {
		return headerDate.UTC(), DateSourceHeader
	}
	for _, receivedDate := range slices.Backward(receivedDates) {
		if IsPlausibleDate(receivedDate, now) {
			return receivedDate.UTC(), DateSourceReceived
		}
	}
	if IsPlausibleDate(fallbackDate, now) {
		return fallbackDate.UTC(), DateSourceFallback
	}
	return time.Time{}, DateSourceNone
}
