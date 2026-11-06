package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type calendarEventsTestStore struct {
	*mockStore

	err   error
	calls int
	query store.CalendarOccurrenceQuery
}

func (s *calendarEventsTestStore) ListCalendarOccurrencesContext(_ context.Context, query store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error) {
	s.calls++
	s.query = query
	return store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{MessageID: 42, Subject: "Planning", Start: query.From, End: query.To}}, Unexpandable: []store.CalendarUnexpandable{}}, s.err
}

func TestCalendarEventsRoute(t *testing.T) {
	backend := &calendarEventsTestStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true, RemoteClients: []config.RemoteClientConfig{{ClientID: "synthetic-reader", APIKey: "synthetic-reader-key"}}}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, secret, _, err := srv.agentGrants.Issue("calendar", []agentgrant.Permission{agentgrant.PermissionCalendarWrite}, []agentgrant.SourceRef{{ID: 1, Type: "gcal", Identifier: "calendar@example.com"}}, time.Time{})
	require.NoError(t, err)
	path := "/api/v1/calendar/events?from=2027-03-01T00:00:00Z&to=2027-04-01T00:00:00Z"
	for _, tc := range []struct {
		name, path, token string
		err               error
		status            int
		want              string
	}{
		{"owner", path + "&limit=7&cursor=next-page", "", nil, 200, `"message_id":42`},
		{"detail", "/api/v1/calendar/events/evt:42", "", store.ErrCalendarOccurrenceNotFound, 404, "not_found"},
		{"delegated", path, secret, nil, 403, "permission_denied"},
		{"remote", path, "synthetic-reader-key", nil, 403, ""},
		{"reversed", "/api/v1/calendar/events?from=2027-04-01T00:00:00Z&to=2027-03-01T00:00:00Z", "", nil, 400, "invalid_argument"},
		{"missing", "/api/v1/calendar/events", "", nil, 400, "invalid_argument"},
		{"broad", path, "", store.ErrQueryTooBroad, 400, "query_too_broad"},
		{"invalid limit", path + "&limit=bad", "", nil, 400, "invalid_argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			backend.err = tc.err
			before := backend.calls
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.name == "remote" {
				r.Header.Set("X-Api-Key", tc.token)
			} else if tc.token != "" {
				r.Header.Set(apiprotocol.AgentTokenHeader, tc.token)
			} else {
				r.Header.Set("X-Api-Key", "synthetic-owner-key")
			}
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.want != "" {
				assertions.Contains(w.Body.String(), tc.want)
			}
			if tc.name == "owner" {
				assertions.Equal(new(7), backend.query.Limit)
				assertions.Equal("next-page", backend.query.Cursor)
			}
			if tc.status == 401 || tc.status == 403 {
				assertions.Equal(before, backend.calls)
			}
		})
	}
}

func TestCalendarOccurrenceGrantReads(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	backend := &calendarEventsScopeStore{Store: testutil.NewTestStore(t)}
	source, err := backend.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	_, err = backend.GetOrCreateSource("gcal", "other@example.com")
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: backend, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}, []agentgrant.SourceRef{{ID: source.ID, Type: "gcal", Identifier: "calendar@example.com"}}, time.Time{})
	requirements.NoError(err)
	for _, path := range []string{"/api/v1/calendar/events?from=2027-03-01T00:00:00Z&to=2027-03-02T00:00:00Z", "/api/v1/calendar/events/evt:42"} {
		backend.scope = nil
		before := backend.calls
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Equal(before+1, backend.calls)
		assertions.Equal([]int64{source.ID}, backend.scope)
	}
}

func (s *calendarEventsTestStore) GetCalendarOccurrenceContext(_ context.Context, _ string) (store.CalendarOccurrenceDetail, error) {
	return store.CalendarOccurrenceDetail{}, s.err
}

type calendarEventsScopeStore struct {
	*store.Store

	scope []int64
	calls int
}

func (s *calendarEventsScopeStore) ListCalendarOccurrencesContext(ctx context.Context, _ store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error) {
	s.scope = sourcescope.IDs(ctx)
	s.calls++
	return store.CalendarOccurrencePage{}, nil
}

func (s *calendarEventsScopeStore) GetCalendarOccurrenceContext(ctx context.Context, _ string) (store.CalendarOccurrenceDetail, error) {
	s.scope = sourcescope.IDs(ctx)
	s.calls++
	return store.CalendarOccurrenceDetail{}, nil
}
