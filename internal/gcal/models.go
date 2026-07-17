// Package gcal is a read-only Google Calendar API v3 client, structured as a
// close mirror of internal/gmail: a hand-rolled net/http client with a dedicated
// rate limiter, an API interface plus in-memory mock, and unexported wire types
// mapped to exported domain types. It deliberately reuses internal/gmail's
// token-bucket RateLimiter (and its adaptive Throttle/RecoverRate backoff) via
// the shared Operation enum and NewRateLimiterWithCapacity.
package gcal

import (
	"encoding/json"
	"time"
)

// Identity constants shared across the calendar sync code.
const (
	// SourceType is the sources.source_type value for calendar sources.
	SourceType = "gcal"
	// AdapterName labels this adapter in logs/metrics.
	AdapterName = "gcal"
	// MessageTypeCalendarEvent is the messages.message_type value for events.
	MessageTypeCalendarEvent = "calendar_event"
	// ConversationType is the conversations.conversation_type value.
	ConversationType = "calendar"
	// RawFormat is the message_raw.raw_format tag for the stored event JSON.
	RawFormat = "gcal_json"
)

// Event status values returned by the API.
const (
	StatusConfirmed = "confirmed"
	StatusTentative = "tentative"
	StatusCancelled = "cancelled"
)

// Calendar is one entry from calendarList.list.
type Calendar struct {
	ID          string `json:"id,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
	TimeZone    string `json:"timeZone,omitempty"`
	AccessRole  string `json:"accessRole,omitempty"` // owner | writer | reader | freeBusyReader
	Primary     bool   `json:"primary,omitempty"`
	Deleted     bool   `json:"deleted,omitempty"`
}

// Person is an organizer/creator reference on an event.
type Person struct {
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Self        bool   `json:"self,omitempty"`
}

// Attendee is one invitee on an event.
type Attendee struct {
	Email          string `json:"email,omitempty"`
	DisplayName    string `json:"displayName,omitempty"`
	ResponseStatus string `json:"responseStatus,omitempty"` // needsAction | declined | tentative | accepted
	Organizer      bool   `json:"organizer,omitempty"`
	Self           bool   `json:"self,omitempty"`
	Resource       bool   `json:"resource,omitempty"`
	Optional       bool   `json:"optional,omitempty"`
}

// EventDateTime is an event start/end. Exactly one of DateTime (timed) or Date
// (all-day) is meaningful. DateTime carries an absolute instant (RFC3339 with
// offset); Date is a calendar day "2006-01-02" with no instant.
type EventDateTime struct {
	DateTime time.Time `json:"dateTime,omitzero"`
	Date     string    `json:"date,omitempty"`
	TimeZone string    `json:"timeZone,omitempty"`
}

// IsAllDay reports whether this is an all-day (date-only) value.
func (e EventDateTime) IsAllDay() bool { return e.Date != "" }

// IsZero reports whether neither a timed nor all-day value is present.
func (e EventDateTime) IsZero() bool { return e.DateTime.IsZero() && e.Date == "" }

// Instant returns a single sortable time for the value, plus ok=false if the
// value is empty/unparseable. Timed events use their absolute DateTime; all-day
// events are normalized to midnight UTC of their Date so they sort and partition
// deterministically regardless of the running machine's timezone.
func (e EventDateTime) Instant() (time.Time, bool) {
	if !e.DateTime.IsZero() {
		return e.DateTime, true
	}
	if e.Date != "" {
		if t, err := time.Parse("2006-01-02", e.Date); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// Event is a single calendar event (a master, a recurring instance, or an
// exception). The full original JSON is preserved separately in message_raw.
type Event struct {
	ID                string        `json:"id,omitempty"`
	Status            string        `json:"status,omitempty"`
	HTMLLink          string        `json:"htmlLink,omitempty"`
	HangoutLink       string        `json:"hangoutLink,omitempty"`
	Created           time.Time     `json:"created,omitzero"`
	Updated           time.Time     `json:"updated,omitzero"`
	Summary           string        `json:"summary,omitempty"`
	Description       string        `json:"description,omitempty"`
	Location          string        `json:"location,omitempty"`
	Creator           Person        `json:"creator,omitzero"`
	Organizer         Person        `json:"organizer,omitzero"`
	Start             EventDateTime `json:"start,omitzero"`
	End               EventDateTime `json:"end,omitzero"`
	Recurrence        []string      `json:"recurrence,omitempty"`       // RRULE/RDATE/EXDATE lines (masters only)
	RecurringEventID  string        `json:"recurringEventId,omitempty"` // set on instances/exceptions of a series
	OriginalStartTime EventDateTime `json:"originalStartTime,omitzero"`
	ICalUID           string        `json:"iCalUID,omitempty"`
	Sequence          int           `json:"sequence,omitempty"`
	Attendees         []Attendee    `json:"attendees,omitempty"`
	Transparency      string        `json:"transparency,omitempty"`
	Visibility        string        `json:"visibility,omitempty"`
	EventType         string        `json:"eventType,omitempty"`

	// Raw is the original API JSON for this event, preserved verbatim for
	// archival fidelity (stored in message_raw). It is not re-serialized from
	// the mapped fields, so fields msgvault does not model (conferenceData,
	// extendedProperties, ...) survive in the archive.
	Raw json.RawMessage `json:"-"`
}

// IsCancelled reports whether the event is a cancellation/tombstone.
func (e Event) IsCancelled() bool { return e.Status == StatusCancelled }

// EventsPage is one page of events.list, plus the pagination/sync tokens. The
// API delivers NextSyncToken only on the final page of a list traversal.
type EventsPage struct {
	Items         []Event `json:"items,omitempty"`
	NextPageToken string  `json:"nextPageToken,omitempty"`
	NextSyncToken string  `json:"nextSyncToken,omitempty"`
	TimeZone      string  `json:"timeZone,omitempty"`
}

// CalendarListPage is one page of calendarList.list.
type CalendarListPage struct {
	Items         []Calendar `json:"items,omitempty"`
	NextPageToken string     `json:"nextPageToken,omitempty"`
}

// EventsListParams configures a single events.list call.
//
// SyncToken and TimeMin/TimeMax are mutually exclusive: the Calendar API rejects
// timeMin/timeMax (and q/orderBy/updatedMin) when syncToken is set, to keep the
// client's incremental state consistent. SingleEvents and ShowDeleted must match
// the values used for the full sync that minted the token; the client always
// forwards them, and full sync uses SingleEvents=false (store masters).
type EventsListParams struct {
	SyncToken    string
	PageToken    string
	SingleEvents bool
	ShowDeleted  bool
	MaxResults   int
	TimeMin      string // RFC3339; full-sync only (ignored when SyncToken set)
	TimeMax      string // RFC3339; full-sync only (ignored when SyncToken set)
}
