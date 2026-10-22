// Package gcal implements Google Calendar API v3 reads and event control, with a
// close mirror of internal/gmail: a hand-rolled net/http client with a dedicated
// rate limiter, an API interface plus in-memory mock, and unexported wire types
// mapped to exported domain types. It deliberately reuses internal/gmail's
// token-bucket RateLimiter (and its adaptive Throttle/RecoverRate backoff) via
// the shared Operation enum and NewRateLimiterWithCapacity.
package gcal

import (
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
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
	RawFormat = gcalmodel.RawFormat
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
	Primary     bool   `json:"primary,omitzero"`
	Deleted     bool   `json:"deleted,omitzero"`
}

type Person = gcalmodel.Person
type Attendee = gcalmodel.Attendee
type EventDateTime = gcalmodel.EventDateTime
type Event = gcalmodel.Event

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
