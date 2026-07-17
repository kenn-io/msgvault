package gcal

import "context"

// CalendarReader enumerates an account's calendars.
type CalendarReader interface {
	// ListCalendars returns one page of the account's calendar list. Pass an
	// empty pageToken for the first page; follow CalendarListPage.NextPageToken
	// until it is empty.
	ListCalendars(ctx context.Context, pageToken string) (*CalendarListPage, error)
}

// EventReader reads events from a single calendar.
type EventReader interface {
	// ListEvents returns one page of events for the given calendar. The traversal
	// is driven by EventsListParams.PageToken; the final page carries
	// EventsPage.NextSyncToken for the next incremental sync. A 410 (expired
	// syncToken) surfaces as *GoneError.
	ListEvents(ctx context.Context, calendarID string, params EventsListParams) (*EventsPage, error)
	// GetEvent fetches a single event by id (used for tombstone reconciliation
	// fallbacks). A missing event surfaces as *NotFoundError.
	GetEvent(ctx context.Context, calendarID, eventID string) (*Event, error)
}

// API is the full read-only Calendar surface the sync layer depends on. The
// concrete *Client and the in-memory *MockAPI both satisfy it.
type API interface {
	CalendarReader
	EventReader
	// Close releases client resources.
	Close() error
}

var (
	_ API = (*Client)(nil)
	_ API = (*MockAPI)(nil)
)
