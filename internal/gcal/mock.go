package gcal

import (
	"context"
	"strconv"
	"sync"
)

// MockAPI is an in-memory, deterministic fake of the Calendar API for tests.
// It owns pagination so tests describe pages as plain event slices (no token
// bookkeeping). It supports multi-page full sync, incremental deltas keyed by
// the incoming syncToken, first-class 410 injection, error injection, and call
// counting. Re-running a full sync (PageToken="") restarts pagination, so
// idempotency tests work without resetting state.
type MockAPI struct {
	mu sync.Mutex

	// Calendars is returned (single page) by ListCalendars.
	Calendars []Calendar

	// FullEvents[calendarID] is the ordered pages of events for a full sync
	// (no syncToken). FullSyncToken[calendarID] is the NextSyncToken delivered
	// on the final full page.
	FullEvents    map[string][][]Event
	FullSyncToken map[string]string

	// IncEvents[incomingSyncToken] is the ordered pages returned when a caller
	// lists with that syncToken. IncNextToken[incomingSyncToken] is the
	// NextSyncToken delivered on the final incremental page.
	IncEvents    map[string][][]Event
	IncNextToken map[string]string

	// GoneTokens[incomingSyncToken]=true makes ListEvents return *GoneError.
	GoneTokens map[string]bool

	// EventsByID[calendarID][eventID] backs GetEvent.
	EventsByID map[string]map[string]Event

	// Injectable errors (returned before any work).
	ListCalendarsErr error
	ListEventsErr    error
	GetEventErr      error

	// Call counters (read under the mutex via the accessor methods).
	listCalendarsCalls int
	listEventsCalls    int
	getEventCalls      int
}

// NewMockAPI returns an empty mock with initialized maps.
func NewMockAPI() *MockAPI {
	return &MockAPI{
		FullEvents:    map[string][][]Event{},
		FullSyncToken: map[string]string{},
		IncEvents:     map[string][][]Event{},
		IncNextToken:  map[string]string{},
		GoneTokens:    map[string]bool{},
		EventsByID:    map[string]map[string]Event{},
	}
}

// ListCalendars returns all seeded calendars in a single page.
func (m *MockAPI) ListCalendars(_ context.Context, _ string) (*CalendarListPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listCalendarsCalls++
	if m.ListCalendarsErr != nil {
		return nil, m.ListCalendarsErr
	}
	items := make([]Calendar, len(m.Calendars))
	copy(items, m.Calendars)
	return &CalendarListPage{Items: items}, nil
}

// ListEvents serves full or incremental pages depending on params.SyncToken.
func (m *MockAPI) ListEvents(_ context.Context, calendarID string, p EventsListParams) (*EventsPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listEventsCalls++
	if m.ListEventsErr != nil {
		return nil, m.ListEventsErr
	}

	if p.SyncToken != "" {
		if m.GoneTokens[p.SyncToken] {
			return nil, &GoneError{Path: "events:" + calendarID}
		}
		return pageAt(m.IncEvents[p.SyncToken], pageIdx(p.PageToken), m.IncNextToken[p.SyncToken]), nil
	}
	return pageAt(m.FullEvents[calendarID], pageIdx(p.PageToken), m.FullSyncToken[calendarID]), nil
}

// GetEvent returns a seeded event by id, or *NotFoundError.
func (m *MockAPI) GetEvent(_ context.Context, calendarID, eventID string) (*Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getEventCalls++
	if m.GetEventErr != nil {
		return nil, m.GetEventErr
	}
	if byID, ok := m.EventsByID[calendarID]; ok {
		if ev, ok := byID[eventID]; ok {
			return &ev, nil
		}
	}
	return nil, &NotFoundError{Path: "events/" + eventID}
}

// Close is a no-op for the mock.
func (m *MockAPI) Close() error { return nil }

// ListCalendarsCalls/ListEventsCalls/GetEventCalls return call counts.
func (m *MockAPI) ListCalendarsCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listCalendarsCalls
}
func (m *MockAPI) ListEventsCalls() int { m.mu.Lock(); defer m.mu.Unlock(); return m.listEventsCalls }
func (m *MockAPI) GetEventCalls() int   { m.mu.Lock(); defer m.mu.Unlock(); return m.getEventCalls }

// pageIdx decodes the mock's pageToken (a stringified index; "" == 0).
func pageIdx(token string) int {
	if token == "" {
		return 0
	}
	if n, err := strconv.Atoi(token); err == nil && n >= 0 {
		return n
	}
	return 0
}

// pageAt returns the page at idx. The final page (or an empty/over-range
// traversal) carries finalToken as NextSyncToken; earlier pages carry a
// NextPageToken pointing at idx+1.
func pageAt(pages [][]Event, idx int, finalToken string) *EventsPage {
	if idx >= len(pages) {
		return &EventsPage{NextSyncToken: finalToken}
	}
	page := &EventsPage{Items: pages[idx]}
	if idx+1 < len(pages) {
		page.NextPageToken = strconv.Itoa(idx + 1)
	} else {
		page.NextSyncToken = finalToken
	}
	return page
}
