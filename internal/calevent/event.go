// Package calevent decodes archived calendar events without archive access.
package calevent

import (
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
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

// JoinKey returns a provider-specific comparison key, preserving no credentials.
// Callers keep the original URL separately for joining the meeting.
func JoinKey(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	path := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	switch {
	case host == "meet.google.com":
		if len(path) == 1 && path[0] != "" {
			return "meet:" + strings.ToLower(path[0])
		}
	case host == "zoom.us" || strings.HasSuffix(host, ".zoom.us"):
		if len(path) == 2 && path[0] == "j" && path[1] != "" {
			for _, c := range path[1] {
				if c < '0' || c > '9' {
					return ""
				}
			}
			return "zoom:" + path[1]
		}
	case host == "teams.microsoft.com":
		escaped := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
		if len(escaped) >= 3 && escaped[0] == "l" && escaped[1] == "meetup-join" && escaped[2] != "" {
			thread, err := url.PathUnescape(escaped[2])
			if err == nil {
				return "teams:" + thread
			}
		}
	}
	return ""
}

// SourceContext provides the calendar identity and current source time zone.
type SourceContext struct{ CalendarID, CalendarTimeZone string }

type Attendee struct {
	Email, DisplayName, Role, RSVP string
	IsCopySelf                     bool
}

type Event struct {
	Provider, Namespace, ProviderEventID, SeriesProviderID, Kind string
	Original                                                     *calrecur.Identity
	ICalUID                                                      string
	Sequence                                                     int64
	Status                                                       string
	HasBounds, AllDay                                            bool
	Start, End                                                   *time.Time
	StartDate, EndDate                                           string
	EventTimeZone, CalendarTimeZone                              string
	Recurrence                                                   []string

	JoinURL              string
	JoinKey              string
	OrganizerEmail       string
	CalendarID           string
	Transparency         string
	Visibility           string
	EventType            string
	Location             string
	CopySelfRSVP         string
	Description          string
	DescriptionTruncated bool
	Attendees            []Attendee
}

type Decoder func(raw, metadata []byte, sc SourceContext) (Event, bool, error)

const maxRawBytes = 64 << 20

var errRawTooLarge = errors.New("calendar event raw exceeds decode limit")

var decoders = struct {
	sync.RWMutex

	byFormat map[string]Decoder
}{byFormat: make(map[string]Decoder)}

// Register installs a decoder for an archived raw format.
func Register(rawFormat string, d Decoder) {
	decoders.Lock()
	defer decoders.Unlock()
	decoders.byFormat[rawFormat] = d
}

// Decode returns ok=false when no decoder accepts the stored raw format.
func Decode(rawFormat string, raw, metadata []byte, sc SourceContext) (Event, bool, error) {
	decoders.RLock()
	decoder := decoders.byFormat[rawFormat]
	decoders.RUnlock()
	if decoder == nil {
		return Event{}, false, nil
	}
	if len(raw) > maxRawBytes {
		return Event{}, false, errRawTooLarge
	}
	return decoder(raw, metadata, sc)
}
