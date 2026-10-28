package loops

import (
	"errors"
	"time"
)

// ParseSnoozeUntil accepts an absolute instant, a date at midnight in now's
// location, or a duration from now.
func ParseSnoozeUntil(value string, now time.Time) (time.Time, error) {
	until, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		until, err = time.ParseInLocation(time.DateOnly, value, now.Location())
	}
	if err != nil {
		var duration time.Duration
		duration, err = time.ParseDuration(value)
		if err == nil {
			until = now.Add(duration)
		}
	}
	if err != nil || !until.After(now) {
		return time.Time{}, errors.New("snooze until must be a future date, RFC3339 instant, or duration")
	}
	return until, nil
}
