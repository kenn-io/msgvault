package api

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personagenda"
)

func TestPersonAgendaHTTPProjectsRecurringTasks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	operations := &fakePersonAgendaOperations{result: personagenda.Result{
		Project: "example", PersonUID: "person-a", PersonUIDs: []string{"person-a"},
		Recurrence: personagenda.RecurrenceStatus{State: "partial", Message: "Some recurring schedules could not be read."},
		Items: []personagenda.Item{
			{UID: "parent", Ref: "abcd", Project: "example", Title: "Review service", Kind: "series", Membership: "direct", Status: "open", State: personagenda.StateOpen, OpenOccurrences: 1, OccurrencesComplete: true,
				Schedules: []personagenda.Schedule{{UID: "job", DefinitionEventUID: "new-event", State: "paused", Summary: "Cron: 0 9 * * 1", Timezone: "Europe/London", Catchup: "latest"}}},
			{UID: "child", Ref: "efgh", Project: "example", Title: "Review service occurrence", Kind: "occurrence", Membership: "inherited", ParentUID: "parent", Status: "open", State: personagenda.StateOpen,
				Occurrence: &personagenda.Occurrence{JobUID: "job", DefinitionEventUID: "old-event", ScheduledAt: "2026-11-02T09:00:00Z"}},
		},
	}}
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), PersonAgendaOperations: operations})
	response := httptest.NewRecorder()
	server.router.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/42/agenda", nil))
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var result map[string]any
	require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	assert.Equal("no-store", response.Header().Get("Cache-Control"))
	assert.Equal(map[string]any{"state": "partial", "message": "Some recurring schedules could not be read."}, result["recurrence"])
	items, ok := result["items"].([]any)
	require.True(ok)
	require.Len(items, 2)
	parent, ok := items[0].(map[string]any)
	require.True(ok)
	assert.Equal("series", parent["kind"])
	assert.Equal(true, parent["occurrences_complete"])
	assert.InDelta(float64(1), parent["open_occurrences"], 0)
	child, ok := items[1].(map[string]any)
	require.True(ok)
	assert.Equal("inherited", child["membership"])
	assert.Equal("parent", child["parent_uid"])
	assert.Equal(map[string]any{"job_uid": "job", "definition_event_uid": "old-event", "scheduled_at": "2026-11-02T09:00:00Z"}, child["occurrence"])
}
