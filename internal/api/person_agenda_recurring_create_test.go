package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/taskclient"
)

func TestRecurringCreateRejectsChangedEndpointBeforeConnecting(t *testing.T) {
	backend := personAgendaBackend{config: config.TaskIntegrationConfig{Endpoint: "http://new.example.test", DefaultProject: "example"}}
	old := config.TaskIntegrationConfig{Endpoint: "http://old.example.test", DefaultProject: "example"}
	_, err := backend.CreateRecurring(t.Context(), 1, "key", personagenda.RecurringCreateInput{Project: "example", Target: recurringTarget(old)})
	require.ErrorIs(t, err, personagenda.ErrRecurringProjectChanged)
}

type fakeRecurringOperations struct {
	fakePersonAgendaOperations

	key string
	err error
}

func TestRecurringCreateReportsAuthenticationAfterParentCreation(t *testing.T) {
	assert := assert.New(t)
	operations := &fakeRecurringOperations{err: taskclient.ErrAuthenticationRequired}
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), PersonAgendaOperations: operations})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/people/42/agenda/recurring", bytes.NewBufferString(`{"title":"Review","owner":"synthetic","settings":{}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "2026-11-08T00:00:00Z/550e8400-e29b-41d4-a716-446655440000")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	assert.Equal(http.StatusAccepted, response.Code)
	assert.Contains(response.Body.String(), `"authentication_required"`)
}

func (f *fakeRecurringOperations) CreateRecurring(_ context.Context, _ int64, key string, _ personagenda.RecurringCreateInput) (personagenda.RecurringCreateResult, error) {
	f.key = key
	err := f.err
	if err == nil {
		err = personagenda.ErrScheduleOutcomeUnknown
	}
	return personagenda.RecurringCreateResult{Parent: personagenda.Item{UID: "parent"}}, err
}

func TestRecurringCreateReturnsPartialParentOnUnknownOutcome(t *testing.T) {
	assert := assert.New(t)
	operations := &fakeRecurringOperations{}
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), PersonAgendaOperations: operations})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/people/42/agenda/recurring", bytes.NewBufferString(`{"title":"Review","owner":"synthetic","settings":{}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "2026-11-08T00:00:00Z/550e8400-e29b-41d4-a716-446655440000")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	assert.Equal(http.StatusAccepted, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), `"outcome_unknown"`)
	assert.Contains(response.Body.String(), `"parent"`)
	assert.Equal("2026-11-08T00:00:00Z/550e8400-e29b-41d4-a716-446655440000", operations.key)
}
