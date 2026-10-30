package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// A cancellation recorded before any details were seen keeps every calendar
// field in the response, as null, so a client can tell unknown from false.
func TestHandleGetMessageCalendarProjection(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	srv, mock := newTestServerWithMockStore(t)
	status := "cancelled"
	mock.messages[0].MessageType = "calendar_event"
	mock.messages[0].Calendar = &store.CalendarProjection{Status: &status}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/1", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	decoder := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&resp))
	assert.Equal(map[string]any{"status": "cancelled", "sequence": nil, "start": nil, "end": nil, "all_day": nil, "time_zone": nil, "ical_uid": nil}, resp["calendar"])
}
