package contactcadence

import "time"

// DueWithinUTCDateHorizon includes the full UTC date of evaluatedAt and each
// subsequent UTC date in days. A zero horizon therefore includes anything due
// before the start of tomorrow UTC.
func DueWithinUTCDateHorizon(dueAt, evaluatedAt time.Time, days int) bool {
	utc := evaluatedAt.UTC()
	cutoff := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, days+1)
	return dueAt.Before(cutoff)
}
