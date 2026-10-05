package daemonclient_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gcal"
)

func TestCalendarControlClientPreservesExplicitEmptyPatch(t *testing.T) {
	assertions := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/calendar/control", r.URL.Path)
		assert.Equal(t, "synthetic-agent-token", r.Header.Get("X-Msgvault-Agent-Token"))
		var body map[string]any
		if !assertions.NoError(json.UnmarshalRead(r.Body, &body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		event, ok := body["event"].(map[string]any)
		if !assertions.True(ok) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.IsType(t, "", event["summary"])
		assert.Empty(t, event["summary"])
		assert.Equal(t, []any{}, event["attendees"])
		assert.Equal(t, []any{}, event["recurrence"])
		assert.NotContains(t, event, "start")
		assert.NotContains(t, event, "end")
		w.Header().Set("Content-Type", "application/json")
		assertions.NoError(json.MarshalWrite(w, calcontrol.Result{Account: "person@example.com", CalendarID: "team@example.com", SendUpdates: "none", Plan: []calcontrol.PlannedWrite{}, Writes: []calcontrol.WriteReceipt{}}))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: "synthetic-agent-token", AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	empty := ""
	attendees := []gcal.Attendee{}
	rules := []string{}
	_, err = client.ControlCalendar(t.Context(), calcontrol.Request{Action: "update", Account: "person@example.com", CalendarID: "team@example.com", EventID: "event", Event: gcal.EventInput{Summary: &empty, Attendees: &attendees, Recurrence: &rules}})
	require.NoError(t, err)
}
