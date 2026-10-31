package api

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
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
	st := testutil.NewTestStore(t)
	sourceIDs, messageIDs := testutil.CalendarArchive(t, st)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}, []agentgrant.SourceRef{{ID: sourceIDs[0], Type: "gcal", Identifier: "calendar0@example.com"}}, time.Time{})
	requirements.NoError(err)
	path := "/api/v1/calendar/events?from=2027-03-01T00:00:00Z&to=2027-03-02T00:00:00Z"
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var page store.CalendarOccurrencePage
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &page))
	requirements.Len(page.Occurrences, 1)
	assertions.Equal(sourceIDs[0], page.Occurrences[0].SourceID)
	r = httptest.NewRequest(http.MethodGet, "/api/v1/calendar/events/"+page.Occurrences[0].Ref, nil)
	r.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	r = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/calendar/events/evt:%d", messageIDs[1]), nil)
	r.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assertions.Equal(http.StatusNotFound, w.Code, w.Body.String())
}

func (s *calendarEventsTestStore) GetCalendarOccurrenceContext(_ context.Context, _ string) (store.CalendarOccurrenceDetail, error) {
	return store.CalendarOccurrenceDetail{}, s.err
}
