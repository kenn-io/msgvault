package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type calendarEventsFixture struct {
	st                                *store.Store
	owner                             *daemonclient.Client
	handler                           http.Handler
	sourceID, hiddenSourceID, eventID int64
}

func calendarEventsAPIFixture(t *testing.T) calendarEventsFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	ids := make([]int64, 0, 2)
	var eventID int64
	for i := range 2 {
		src, err := st.GetOrCreateSource("gcal", fmt.Sprintf("calendar%d@example.com", i))
		require.NoError(t, err)
		require.NoError(t, st.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"primary","time_zone":"UTC"}`))
		ids = append(ids, src.ID)
		conv, err := st.EnsureConversation(src.ID, "event", "Planning")
		require.NoError(t, err)
		id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "event", MessageType: "calendar_event", Subject: sql.NullString{String: "Planning", Valid: true}})
		require.NoError(t, err)
		require.NoError(t, st.UpsertMessageRawWithFormat(id, []byte(`{"id":"event","status":"confirmed","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"}}`), "gcal_json"))
		if i == 0 {
			eventID = id
		}
	}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	return calendarEventsFixture{st: st, owner: owner, handler: srv.Router(), sourceID: ids[0], hiddenSourceID: ids[1], eventID: eventID}
}

func calendarEventsAgentRequest(t *testing.T, owner *daemonclient.Client, handler http.Handler, permission string, ids []int64, path string) *httptest.ResponseRecorder {
	t.Helper()
	token, err := owner.IssueAgentToken(t.Context(), "reader", []string{permission}, ids, nil, time.Time{})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, token.Secret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

const calendarEventsTestPath = "/api/v1/calendar/events?from=2027-03-01T00:00:00Z&to=2027-03-02T00:00:00Z"

func TestCalendarEventsMessageReadAllowed(t *testing.T) {
	f := calendarEventsAPIFixture(t)
	owner, handler, src, id := f.owner, f.handler, f.sourceID, f.eventID
	w := calendarEventsAgentRequest(t, owner, handler, "message.read", []int64{src}, calendarEventsTestPath)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), fmt.Sprintf(`"ref":"evt:%d"`, id))
	w = calendarEventsAgentRequest(t, owner, handler, "message.read", []int64{src}, fmt.Sprintf("/api/v1/calendar/events/evt:%d", id))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestCalendarEventsCalendarReadOnlyDenied(t *testing.T) {
	f := calendarEventsAPIFixture(t)
	owner, handler, src := f.owner, f.handler, f.sourceID
	w := calendarEventsAgentRequest(t, owner, handler, "calendar.read", []int64{src}, calendarEventsTestPath)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

func TestCalendarEventsGrantNarrowsSources(t *testing.T) {
	assertions := assert.New(t)
	f := calendarEventsAPIFixture(t)
	owner, handler, src, hidden, id := f.owner, f.handler, f.sourceID, f.hiddenSourceID, f.eventID
	w := calendarEventsAgentRequest(t, owner, handler, "message.read", []int64{src}, calendarEventsTestPath)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assertions.Contains(w.Body.String(), fmt.Sprintf(`"source_id":%d`, src))
	assertions.NotContains(w.Body.String(), fmt.Sprintf(`"source_id":%d`, hidden))
	w = calendarEventsAgentRequest(t, owner, handler, "message.read", []int64{hidden}, fmt.Sprintf("/api/v1/calendar/events/evt:%d", id))
	assertions.Equal(http.StatusNotFound, w.Code, w.Body.String())
}

func TestCalendarEventsZeroGrantedSources(t *testing.T) {
	requirements := require.New(t)
	f := calendarEventsAPIFixture(t)
	st, owner, handler, src := f.st, f.owner, f.handler, f.sourceID
	token, err := owner.IssueAgentToken(t.Context(), "reader", []string{"message.read"}, []int64{src}, nil, time.Time{})
	requirements.NoError(err)
	// Removing the source leaves a valid grant whose selector matches nothing.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE sources SET identifier = ? WHERE id = ?"), "renamed@example.com", src)
	requirements.NoError(err)
	req := httptest.NewRequest(http.MethodGet, calendarEventsTestPath, nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, token.Secret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"items":[]`)
}

func TestCalendarEventsInvalidQuery(t *testing.T) {
	handler := calendarEventsAPIFixture(t).handler
	for _, query := range []string{
		"from=invalid&to=2027-03-02T00:00:00Z",
		"from=2027-03-01T00:00:00Z&to=2027-03-02T00:00:00Z&limit=0",
		"from=2027-03-01T00:00:00Z&to=2027-03-02T00:00:00Z&limit=501",
		"from=2027-03-01T00:00:00Z&to=2027-03-02T00:00:00Z&source_ids=0",
		"from=2027-03-01T00:00:00Z&to=2027-03-02T00:00:00Z&include_cancelled=invalid",
		"from=2027-03-02T00:00:00Z&to=2027-03-01T00:00:00Z",
	} {
		t.Run(query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/calendar/events?"+query, nil)
			req.Header.Set("X-Api-Key", "owner")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
}
