package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

	err       error
	calls     int
	subject   string
	sourceIDs []int64
}

func (s *calendarEventsTestStore) ListCalendarOccurrencesContext(ctx context.Context, from, to time.Time) (store.CalendarOccurrencePage, error) {
	s.calls++
	s.sourceIDs = sourcescope.IDs(ctx)
	return store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{MessageID: 42, Subject: s.subject, Start: from, End: to}}, Unexpandable: []store.CalendarUnexpandable{}, Coverage: &store.CalendarCoverage{Notice: "Archive only", Sources: []store.CalendarSourceCoverage{{SourceID: 1, Name: "Work", Status: "no_baseline"}}}}, s.err
}

func (s *calendarEventsTestStore) ListSourcesContext(context.Context, string) ([]*store.Source, error) {
	return []*store.Source{{ID: 1, SourceType: "gcal", Identifier: "allowed@example.test"}, {ID: 2, SourceType: "gcal", Identifier: "other@example.test"}}, nil
}

func (s *calendarEventsTestStore) BeginReadSnapshotContext(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func (s *calendarEventsTestStore) DB() *sql.DB        { return nil }
func (s *calendarEventsTestStore) IsPostgreSQL() bool { return false }
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
		want              []string
	}{
		{"owner", path, "", nil, 200, []string{`"message_id":42`, `"status":"no_baseline"`}},
		{"delegated without calendar.event.read", path, secret, nil, 403, nil},
		{"remote", path, "synthetic-reader-key", nil, 403, nil},
		{"missing", "/api/v1/calendar/events", "", nil, 400, []string{"invalid_argument"}},
		{"reversed", strings.Replace(path, "2027-04-01", "2027-02-01", 1), "", nil, 400, []string{"invalid_argument"}},
		{"broad", path, "", store.ErrQueryTooBroad, 400, []string{"query_too_broad"}},
		{"series cap", path, "", fmt.Errorf("%w: more than 2000 recurring series overlap the window; a narrower window will not reduce open-ended series", store.ErrQueryTooBroad), 400, []string{"more than 2000 recurring series overlap the window; a narrower window will not reduce open-ended series"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend.err = tc.err
			before := backend.calls
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil)
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
			for _, want := range tc.want {
				assert.Contains(t, w.Body.String(), want)
			}
			if tc.status == 401 || tc.status == 403 {
				assert.Equal(t, before, backend.calls)
			}
		})
	}
}

func TestCalendarEventsAgentScopeAndVerification(t *testing.T) {
	backend := &calendarEventsTestStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: backend, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	source := agentgrant.SourceRef{ID: 1, Type: "gcal", Identifier: "allowed@example.test"}
	t.Run("ungranted source filter", func(t *testing.T) {
		_, token, _, err := srv.agentGrants.Issue("filtered", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}, []agentgrant.SourceRef{source}, time.Time{})
		require.NoError(t, err)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/calendar/events?from=2027-03-02T00:00:00Z&to=2027-03-03T00:00:00Z&source_id=2", nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	})
	for _, tc := range []struct {
		name, subject, want string
		permissions         []agentgrant.Permission
	}{
		{"scoped", "Your verification code is 482913", "[verification content masked]", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}},
		{"reveal", "Your verification code is 482913", "Your verification code is 482913", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead, agentgrant.PermissionVerificationRead}},
		{"ordinary meeting", "Weekly sync", "Weekly sync", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			backend.subject = tc.subject
			_, token, _, err := srv.agentGrants.Issue(tc.name, tc.permissions, []agentgrant.SourceRef{source}, time.Time{})
			requirements.NoError(err)
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/calendar/events?from=2027-03-02T00:00:00Z&to=2027-03-03T00:00:00Z", nil)
			r.Header.Set(apiprotocol.AgentTokenHeader, token)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			var page store.CalendarOccurrencePage
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &page))
			requirements.Len(page.Occurrences, 1)
			assert.Equal(t, tc.want, page.Occurrences[0].Subject)
			assert.Equal(t, []int64{source.ID}, backend.sourceIDs)
		})
	}
}

func TestCalendarEventsEditedOccurrence(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gcal", "allowed@example.test")
	requirements.NoError(err)
	for _, tc := range []struct{ id, subject, raw string }{
		{"series", "Your verification code is 482913", `{"id":"series","start":{"dateTime":"2027-03-04T17:00:00Z"},"end":{"dateTime":"2027-03-04T18:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=4"]}`},
		{"edited", "Planning", `{"id":"edited","recurringEventId":"series","status":"confirmed","originalStartTime":{"dateTime":"2027-03-07T17:00:00Z"},"start":{"dateTime":"2027-03-07T17:00:00Z"},"end":{"dateTime":"2027-03-07T18:00:00Z"}}`},
	} {
		_, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: tc.id, MessageType: "calendar_event", Subject: sql.NullString{String: tc.subject, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: tc.id, ConversationType: "calendar", Title: tc.subject},
			RawMIME:      []byte(tc.raw), RawFormat: "gcal_json", Metadata: &sql.NullString{String: `{"calendar_id":"example.test"}`, Valid: true},
		})
		requirements.NoError(err)
	}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	_, token, _, err := srv.agentGrants.Issue("edited", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}, []agentgrant.SourceRef{{ID: source.ID, Type: "gcal", Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/calendar/events?from=2027-03-07T00:00:00Z&to=2027-03-08T00:00:00Z", nil)
	r.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var page store.CalendarOccurrencePage
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &page))
	requirements.Len(page.Occurrences, 1)
	assert.Equal(t, "Planning", page.Occurrences[0].Subject)
}

func TestCalendarEventsAgentSourceScopeRealStore(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	allowed, err := st.GetOrCreateSource("gcal", "allowed@example.test")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("gcal", "other@example.test")
	requirements.NoError(err)
	for _, src := range []*store.Source{allowed, other} {
		for _, event := range []struct{ id, subject, raw string }{
			{"series", src.Identifier + " series", `{"id":"series","start":{"dateTime":"2027-03-04T17:00:00Z"},"end":{"dateTime":"2027-03-04T18:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=2"]}`},
			{"edited", src.Identifier + " edited", `{"id":"edited","recurringEventId":"series","status":"confirmed","originalStartTime":{"dateTime":"2027-03-05T17:00:00Z"},"start":{"dateTime":"2027-03-05T17:00:00Z"},"end":{"dateTime":"2027-03-05T18:00:00Z"}}`},
		} {
			_, err := st.PersistMessage(&store.MessagePersistData{
				Message:      &store.Message{SourceID: src.ID, SourceMessageID: event.id, MessageType: "calendar_event", Subject: sql.NullString{String: event.subject, Valid: true}},
				Conversation: &store.ConversationPersistData{SourceConversationID: event.id, ConversationType: "calendar", Title: event.subject},
				RawMIME:      []byte(event.raw), RawFormat: "gcal_json", Metadata: &sql.NullString{String: `{"calendar_id":"example.test"}`, Valid: true},
			})
			requirements.NoError(err)
		}
	}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	_, token, _, err := srv.agentGrants.Issue("calendar", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}, []agentgrant.SourceRef{{ID: allowed.ID, Type: "gcal", Identifier: allowed.Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, tc := range []struct {
		name, header, token string
		want                map[string]int64
	}{
		{"agent", apiprotocol.AgentTokenHeader, token, map[string]int64{allowed.Identifier + " series": allowed.ID, allowed.Identifier + " edited": allowed.ID}},
		{"owner", "X-Api-Key", "owner", map[string]int64{allowed.Identifier + " series": allowed.ID, allowed.Identifier + " edited": allowed.ID, other.Identifier + " series": other.ID, other.Identifier + " edited": other.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/calendar/events?from=2027-03-04T00:00:00Z&to=2027-03-06T00:00:00Z", nil)
			r.Header.Set(tc.header, tc.token)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			var page store.CalendarOccurrencePage
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &page))
			requirements.Len(page.Occurrences, len(tc.want))
			got := make(map[string]int64, len(page.Occurrences))
			for _, occurrence := range page.Occurrences {
				got[occurrence.Subject] = occurrence.SourceID
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
