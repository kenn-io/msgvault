package gcalmodel

import (
	"encoding/json/jsontext"
	"strings"
	"time"
)

// RawFormat identifies preserved Google Calendar event JSON.
const RawFormat = "gcal_json"

// Person is an organizer/creator reference on an event.
type Person struct {
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Self        bool   `json:"self,omitzero"`
}

// Attendee is one invitee on an event.
type Attendee struct {
	Email            string `json:"email,omitempty"`
	DisplayName      string `json:"displayName,omitempty"`
	ResponseStatus   string `json:"responseStatus,omitempty"` // needsAction | declined | tentative | accepted
	Organizer        bool   `json:"organizer,omitzero"`
	Self             bool   `json:"self,omitzero"`
	Resource         bool   `json:"resource,omitzero"`
	Optional         bool   `json:"optional,omitzero"`
	AdditionalGuests int    `json:"additionalGuests,omitzero"`
	Comment          string `json:"comment,omitempty"`
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
	ETag              string        `json:"etag,omitempty"`
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
	Sequence          int           `json:"sequence,omitzero"`
	Attendees         []Attendee    `json:"attendees,omitempty"`
	Transparency      string        `json:"transparency,omitempty"`
	Visibility        string        `json:"visibility,omitempty"`
	EventType         string        `json:"eventType,omitempty"`
	Reminders         *Reminders    `json:"reminders,omitzero"`

	// Raw is the original API JSON for this event, preserved verbatim for
	// archival fidelity (stored in message_raw). It is not re-serialized from
	// the mapped fields, so fields msgvault does not model (conferenceData,
	// extendedProperties, ...) survive in the archive.
	Raw jsontext.Value `json:"-"`
}

// IsCancelled reports whether the event is a cancellation/tombstone.
func (e Event) IsCancelled() bool { return e.Status == "cancelled" }

type Reminder struct {
	Method  string `json:"method"`
	Minutes int    `json:"minutes"`
}
type Reminders struct {
	UseDefault bool       `json:"useDefault"`
	Overrides  []Reminder `json:"overrides"`
}

// NormalizeParticipantEmail is the comparison form used for calendar participants.
func NormalizeParticipantEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
