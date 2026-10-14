package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type calendarEventsErrorBackend struct {
	*store.Store

	err error
}

func (b calendarEventsErrorBackend) ListCalendarOccurrencesContext(context.Context, store.SourceScope, store.CalendarOccurrenceFilter) (store.CalendarOccurrencePage, error) {
	return store.CalendarOccurrencePage{}, b.err
}
func (b calendarEventsErrorBackend) GetCalendarOccurrenceContext(context.Context, store.SourceScope, string) (store.CalendarOccurrence, error) {
	return store.CalendarOccurrence{}, b.err
}

func TestCalendarEventsErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"too broad", store.ErrQueryTooBroad, 400, "query_too_broad"},
		{"invalid", store.ErrInvalidArgument, 400, "invalid_argument"},
		{"hidden", store.ErrNotFound, 404, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := calendarEventsErrorBackend{Store: testutil.NewTestStore(t), err: tc.err}
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Store: backend, Logger: testLogger()})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			paths := []string{"/api/v1/calendar/events?from=2027-03-01T00:00:00Z&to=2027-03-02T00:00:00Z", "/api/v1/calendar/events/evt:1"}
			if errors.Is(tc.err, store.ErrNotFound) {
				paths = paths[1:]
			}
			for _, path := range paths {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set("X-Api-Key", "owner")
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, req)
				assert.Equal(t, tc.status, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), tc.code)
			}
		})
	}
}

func TestGetCalendarEventUnsupportedRecurrenceIsUnprocessable(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"primary","time_zone":"UTC"}`))
	conversation, err := st.EnsureConversation(src.ID, "series", "Calendar fixture")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conversation,
		SourceMessageID: "series", MessageType: "calendar_event"})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(`{"id":"series","summary":"Planning","status":"confirmed","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"recurrence":["RRULE:FREQ=HOURLY;COUNT=3"]}`), "gcal_json"))

	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	req := httptest.NewRequest(http.MethodGet, "/api/v1/calendar/events/evt:"+strconv.FormatInt(id, 10)+"@20270301T100000Z", nil)
	req.Header.Set("X-Api-Key", "owner")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assertions.Equal(http.StatusUnprocessableEntity, w.Code, w.Body.String())
	assertions.Contains(w.Body.String(), "recurrence_unsupported")
}
