// Package calevent decodes archived calendar events without archive access.
package calevent

import (
	"context"
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
	// StartWall stores the supplied local DTSTART fields as a UTC timestamp.
	// It preserves wall times that resolve through a daylight-saving gap.
	StartWall                       *time.Time `json:"start_wall,omitempty"`
	StartDate, EndDate              string
	EventTimeZone, CalendarTimeZone string
	Recurrence                      []string
	RecurrenceError                 string
	Attendees                       []Attendee
}

// Decode reads a provider snapshot with the source calendar's time zone.
func Decode(rawFormat string, raw, metadata []byte, calendarZone string) (Event, bool, error) {
	switch rawFormat {
	case "ics":
		return DecodeICS(context.Background(), raw, metadata, calendarZone, NewICSParseBudget())
	case gcalmodel.RawFormat:
		return decodeGoogleEvent(raw, metadata, calendarZone)
	default:
		return Event{}, false, nil
	}
}
