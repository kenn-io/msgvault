package calevent

import (
	"fmt"
	"time"

	ical "github.com/emersion/go-ical"
)

// icsEventZones names the zones an event's properties reference.
func icsEventZones(event *ical.Component) map[string]bool {
	used := make(map[string]bool)
	for _, values := range event.Props {
		for _, prop := range values {
			used[normalizeICSZone(prop.Params.Get("TZID"))] = true
		}
	}
	return used
}

// LoadZone uses Go time zone rules; embedded VTIMEZONE rules are ignored.
func LoadZone(name string) (*time.Location, error) {
	if name == "Local" {
		return nil, fmt.Errorf("load time zone %q: host-dependent", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("load time zone %q: %w", name, err)
	}
	return loc, nil
}
