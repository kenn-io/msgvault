// Package calrecur expands calendar recurrence sets without storing occurrences.
package calrecur

import (
	"time"
)

const dateLayout = "2006-01-02"
const keyDateLayout = "20060102"
const keyInstantLayout = "20060102T150405Z"

// Identity preserves a civil date for all-day events and a UTC instant otherwise.
type Identity struct {
	Instant time.Time
	Date    string
	AllDay  bool
}

func (i Identity) Key() string {
	if i.AllDay {
		d, err := time.Parse(dateLayout, i.Date)
		if err != nil {
			return ""
		}
		return d.Format(keyDateLayout)
	}
	return i.Instant.UTC().Format(keyInstantLayout)
}
