// Package calevent decodes archived calendar events without archive access.
package calevent

import (
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

// IsRoomAttendee identifies provider resource addresses and explicit resources.
func IsRoomAttendee(email string, resourceFlag bool) bool {
	if resourceFlag {
		return true
	}
	email = strings.ToLower(strings.TrimSpace(email))
	_, domain, ok := strings.Cut(email, "@")
	return ok && (domain == "resource.calendar.google.com" || strings.HasSuffix(domain, ".resource.calendar.google.com"))
}

type Attendee struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	RSVP        string `json:"rsvp"`
	IsCopySelf  bool   `json:"is_self"`
}
type Event struct {
	ProviderEventID, SeriesProviderID, Kind string
	Original                                *calrecur.Identity
	Status                                  string
	AllDay                                  bool
	Start, End                              *time.Time
	StartDate, EndDate                      string
	EventTimeZone, CalendarTimeZone         string
	Recurrence                              []string
	Attendees                               []Attendee
}

// Decode reads a provider snapshot with the source calendar's time zone.
func Decode(rawFormat string, raw, metadata []byte, calendarZone string) (Event, bool, error) {
	switch rawFormat {
	case gcalmodel.RawFormat:
		return decodeGoogleEvent(raw, metadata, calendarZone)
	default:
		return Event{}, false, nil
	}
}
