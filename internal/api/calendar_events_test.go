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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type calendarEventsTestStore struct {
	*mockStore

	err   error
	calls int
}

func (s *calendarEventsTestStore) ListCalendarOccurrencesContext(_ context.Context, from, to time.Time) (store.CalendarOccurrencePage, error) {
	s.calls++
	return store.CalendarOccurrencePage{Occurrences: []store.CalendarOccurrence{{MessageID: 42, Subject: "Planning", Start: from, End: to}}, Unexpandable: []store.CalendarUnexpandable{}}, s.err
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
		{"owner", path, "", nil, 200, `"message_id":42`},
		{"delegated", path, secret, nil, 403, ""},
		{"remote", path, "synthetic-reader-key", nil, 403, ""},
		{"missing", "/api/v1/calendar/events", "", nil, 400, "invalid_argument"},
		{"reversed", strings.Replace(path, "2027-04-01", "2027-02-01", 1), "", nil, 400, "invalid_argument"},
		{"broad", path, "", store.ErrQueryTooBroad, 400, "query_too_broad"},
		{"series cap", path, "", fmt.Errorf("%w: more than 2000 recurring series overlap the window; a narrower window will not reduce open-ended series", store.ErrQueryTooBroad), 400, "more than 2000 recurring series overlap the window; a narrower window will not reduce open-ended series"},
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
			if tc.want != "" {
				assert.Contains(t, w.Body.String(), tc.want)
			}
			if tc.status == 401 || tc.status == 403 {
				assert.Equal(t, before, backend.calls)
			}
		})
	}
}

func TestCalendarEventsAgentScopeAndVerification(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gcal", "allowed@example.test")
	require.NoError(t, err)
	for _, tc := range []struct{ id, subject, raw, body string }{
		{"event", "Your verification code is 482913", `{"id":"event","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"}}`, ""},
		{"series", "Your verification code is 482913", `{"id":"series","start":{"dateTime":"2027-03-04T17:00:00Z"},"end":{"dateTime":"2027-03-04T18:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=4"]}`, ""},
		{"cancelled", "", `{"id":"cancelled","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-05T17:00:00Z"}}`, ""},
		{"edited", "Planning", `{"id":"edited","recurringEventId":"series","status":"confirmed","originalStartTime":{"dateTime":"2027-03-07T17:00:00Z"},"start":{"dateTime":"2027-03-07T17:00:00Z"},"end":{"dateTime":"2027-03-07T18:00:00Z"}}`, ""},
		{"meeting", "Weekly sync", `{"id":"meeting","start":{"dateTime":"2027-03-08T17:00:00Z"},"end":{"dateTime":"2027-03-08T18:00:00Z"}}`, "Zoom meeting ID: 812 3456 7890. Passcode: 482913"},
	} {
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: tc.id, MessageType: "calendar_event", Subject: sql.NullString{String: tc.subject, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: tc.id, ConversationType: "calendar", Title: tc.subject},
			RawMIME:      []byte(tc.raw), RawFormat: "gcal_json", Metadata: &sql.NullString{String: `{"calendar_id":"example.test"}`, Valid: true},
		})
		require.NoError(t, err)
		if tc.body != "" {
			require.NoError(t, st.UpsertMessageBodyContext(t.Context(), id, sql.NullString{String: tc.body, Valid: true}, sql.NullString{}))
		}
	}
	other, err := st.GetOrCreateSource("gcal", "other@example.test")
	require.NoError(t, err)
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: other.ID, SourceMessageID: "private", MessageType: "calendar_event", Subject: sql.NullString{String: "Private meeting", Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "private", ConversationType: "calendar", Title: "Private meeting"},
		RawMIME:      []byte(`{"id":"private","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"}}`), RawFormat: "gcal_json", Metadata: &sql.NullString{String: `{"calendar_id":"example.test"}`, Valid: true},
	})
	require.NoError(t, err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	for _, tc := range []struct {
		name, from, to, want, status string
		permissions                  []agentgrant.Permission
	}{
		{"scoped", "2027-03-02", "2027-03-03", "[verification content masked]", "confirmed", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}},
		{"reveal", "2027-03-02", "2027-03-03", "Your verification code is 482913", "confirmed", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead, agentgrant.PermissionVerificationRead}},
		{"inherited cancellation", "2027-03-05", "2027-03-06", "[verification content masked]", "cancelled", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}},
		{"edited occurrence", "2027-03-07", "2027-03-08", "Planning", "confirmed", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}},
		{"ordinary meeting", "2027-03-08", "2027-03-09", "Weekly sync", "confirmed", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, token, _, err := srv.agentGrants.Issue(tc.name, tc.permissions, []agentgrant.SourceRef{{ID: source.ID, Type: "gcal", Identifier: source.Identifier}}, time.Time{})
			require.NoError(t, err)
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/calendar/events?from="+tc.from+"T00:00:00Z&to="+tc.to+"T00:00:00Z", nil)
			r.Header.Set(apiprotocol.AgentTokenHeader, token)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var page store.CalendarOccurrencePage
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
			require.Len(t, page.Occurrences, 1)
			assert.Equal(t, tc.want, page.Occurrences[0].Subject)
			assert.Equal(t, tc.status, page.Occurrences[0].Status)
			assert.NotContains(t, w.Body.String(), "Private meeting")
		})
	}
}
