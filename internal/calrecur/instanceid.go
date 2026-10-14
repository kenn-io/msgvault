// Package calrecur expands calendar recurrence sets without storing occurrences.
package calrecur

import (
	"fmt"
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

func ParseKey(key string) (Identity, error) {
	switch len(key) {
	case len(keyDateLayout):
		d, err := time.Parse(keyDateLayout, key)
		if err == nil && d.Format(keyDateLayout) == key {
			return Identity{Date: d.Format(dateLayout), AllDay: true}, nil
		}
	case len(keyInstantLayout):
		d, err := time.Parse(keyInstantLayout, key)
		if err == nil && d.Format(keyInstantLayout) == key {
			return Identity{Instant: d.UTC()}, nil
		}
	}
	return Identity{}, fmt.Errorf("invalid occurrence key %q", key)
}

func FormatInstanceID(base string, id Identity) string { return base + "_" + id.Key() }

func ParseInstanceID(s string) (base string, id Identity, ok bool) {
	index := strings.LastIndexByte(s, '_')
	if index <= 0 {
		return "", Identity{}, false
	}
	base, key := s[:index], s[index+1:]
	id, err := ParseKey(key)
	return base, id, err == nil
}
