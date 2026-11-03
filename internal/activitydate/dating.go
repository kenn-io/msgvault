// Package activitydate dates archived communication without archive dependencies.
package activitydate

import (
	"errors"
	"fmt"
	"time"
)

type DateOrigin string

const (
	OriginSentAt       DateOrigin = "sent_at"
	OriginReceivedAt   DateOrigin = "received_at"
	OriginInternalDate DateOrigin = "internal_date"
)

type DatePrecision string

const (
	// PrecisionTimestamp means the chosen instant has sub-day resolution.
	PrecisionTimestamp DatePrecision = "timestamp"
	// PrecisionDay means the source explicitly supplied a calendar date rather
	// than a timestamp. Shifting it into another zone would invent information,
	// so day-precision events are always dated in UTC.
	PrecisionDay DatePrecision = "day"
)

// Timestamps holds the three candidate instants an archived message carries.
// The coalesce order matches the rest of the archive. DateOnly describes the
// selected source value; callers set it from explicit source metadata such as
// an all-day calendar event, never by inspecting the instant's clock fields.
type Timestamps struct {
	SentAt       *time.Time
	ReceivedAt   *time.Time
	InternalDate *time.Time
	DateOnly     bool
}

// Dating is the materialized dating metadata for one projected event.
type Dating struct {
	OccurredAt       time.Time
	Origin           DateOrigin
	Precision        DatePrecision
	Timezone         string
	UTCOffsetMinutes int
	LocalDate        string
}

var ErrNoTimestamp = errors.New("activity: message has no usable timestamp")

// LoadZone resolves an IANA zone name. An empty name means UTC.
func LoadZone(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("activity: invalid timezone %q: %w", name, err)
	}
	return zone, nil
}

// DateEvent selects the event's instant, applies its explicit precision, and
// materializes the local date plus the exact offset used to derive it.
func DateEvent(ts Timestamps, loc *time.Location) (Dating, error) {
	instant, origin, ok := pickInstant(ts)
	if !ok {
		return Dating{}, ErrNoTimestamp
	}
	utc := instant.UTC()

	precision := PrecisionTimestamp
	zone := loc
	if zone == nil {
		zone = time.UTC
	}
	if ts.DateOnly {
		precision = PrecisionDay
		zone = time.UTC
	}

	local := utc.In(zone)
	_, offsetSeconds := local.Zone()
	return Dating{
		OccurredAt:       utc,
		Origin:           origin,
		Precision:        precision,
		Timezone:         zone.String(),
		UTCOffsetMinutes: offsetSeconds / 60,
		LocalDate:        local.Format(time.DateOnly),
	}, nil
}

// Compare orders activity dates with the precision supplied by their source.
// Day-precision values are UTC-anchored civil dates; timestamps use the
// projection timezone. When either side is day-precision, compare those civil
// dates instead of treating the UTC anchor as an instant.
func Compare(a time.Time, aPrecision DatePrecision, b time.Time, bPrecision DatePrecision, loc *time.Location) int {
	if aPrecision == PrecisionDay || bPrecision == PrecisionDay {
		if loc == nil {
			loc = time.UTC
		}
		civil := func(at time.Time, precision DatePrecision) time.Time {
			if precision != PrecisionDay {
				at = at.In(loc)
			} else {
				at = at.UTC()
			}
			return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		}
		a, b = civil(a, aPrecision), civil(b, bPrecision)
	}
	return a.Compare(b)
}

func pickInstant(ts Timestamps) (time.Time, DateOrigin, bool) {
	switch {
	case usable(ts.SentAt):
		return *ts.SentAt, OriginSentAt, true
	case usable(ts.ReceivedAt):
		return *ts.ReceivedAt, OriginReceivedAt, true
	case usable(ts.InternalDate):
		return *ts.InternalDate, OriginInternalDate, true
	default:
		return time.Time{}, "", false
	}
}

func usable(value *time.Time) bool {
	return value != nil && !value.IsZero()
}
