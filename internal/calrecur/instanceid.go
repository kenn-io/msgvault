// Package calrecur expands calendar recurrence sets without storing occurrences.
package calrecur

import (
	"strings"
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

// ParseInstanceID splits a Google instance ID into its base and original identity.
func ParseInstanceID(value string) (string, Identity, bool) {
	index := strings.LastIndexByte(value, '_')
	if index <= 0 {
		return "", Identity{}, false
	}
	base, key := value[:index], value[index+1:]
	for _, layout := range []string{keyDateLayout, keyInstantLayout} {
		at, err := time.Parse(layout, key)
		if err != nil || at.Format(layout) != key {
			continue
		}
		if layout == keyDateLayout {
			return base, Identity{Date: at.Format(dateLayout), AllDay: true}, true
		}
		return base, Identity{Instant: at.UTC()}, true
	}
	return "", Identity{}, false
}
