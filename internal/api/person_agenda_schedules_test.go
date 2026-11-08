package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personagenda"
)

func TestUnsupportedScheduleReportsManagementInKata(t *testing.T) {
	assert := assert.New(t)
	response := httptest.NewRecorder()
	writeScheduleError(response, personagenda.ErrUnsupportedSchedule)
	assert.Equal(http.StatusUnprocessableEntity, response.Code)
	assert.Contains(response.Body.String(), "unsupported_schedule")
	assert.Contains(response.Body.String(), "managed in Kata")
}

type fakeScheduleOperations struct {
	fakePersonAgendaOperations

	personID        int64
	parentID, jobID string
	input           personagenda.ScheduleUpdateInput
}

func (f *fakeScheduleOperations) UpdateSchedule(_ context.Context, personID int64, parentID, jobID string, input personagenda.ScheduleUpdateInput) (personagenda.Schedule, error) {
	f.personID, f.parentID, f.jobID, f.input = personID, parentID, jobID, input
	return personagenda.Schedule{UID: jobID, State: "paused"}, nil
}

func TestPersonAgendaScheduleActionPassesScopedIdentity(t *testing.T) {
	assert := assert.New(t)
	operations := &fakeScheduleOperations{}
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), PersonAgendaOperations: operations})
	response := personRequest(t, server, http.MethodPost, "/api/v1/people/42/agenda/parent/schedules/01ARZ3NDEKTSV4RRFFQ69G5FAV/actions", []byte(`{"action":"pause","expected_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW"}`), "")
	assert.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal(int64(42), operations.personID)
	assert.Equal("parent", operations.parentID)
	assert.Equal("01ARZ3NDEKTSV4RRFFQ69G5FAV", operations.jobID)
	assert.Equal("pause", operations.input.Action)
	assert.Equal("01ARZ3NDEKTSV4RRFFQ69G5FAW", operations.input.ExpectedEventUID)
}

func TestPersonAgendaScheduleActionsHaveScopedRoute(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), PersonAgendaOperations: &fakePersonAgendaOperations{}})
	response := personRequest(t, server, http.MethodPost, "/api/v1/people/42/agenda/parent/schedules/01ARZ3NDEKTSV4RRFFQ69G5FAV/actions", []byte(`{"action":"pause","expected_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW"}`), "")
	assert.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	path := OpenAPIDocument().Paths["/api/v1/people/{id}/agenda/{ref}/schedules/{job_uid}/actions"]
	require.NotNil(path)
	require.NotNil(path.Post)
	assert.Equal("updatePersonAgendaSchedule", path.Post.OperationID)
	for _, code := range []string{"400", "401", "403", "404", "409", "422", "503"} {
		assert.Contains(path.Post.Responses, code)
	}
}
