package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/taskclient"
)

type fakePersonAgendaOperations struct {
	result   personagenda.Result
	err      error
	created  personagenda.CreateInput
	updated  personagenda.UpdateInput
	linkedID string
	unlinked string
	key      string
}

func (f *fakePersonAgendaOperations) List(context.Context, int64) (personagenda.Result, error) {
	return f.result, f.err
}
func (f *fakePersonAgendaOperations) Create(_ context.Context, _ int64, key string, input personagenda.CreateInput) (personagenda.Item, error) {
	f.key, f.created = key, input
	return personagenda.Item{UID: "01NEW", Ref: "new", QualifiedRef: "msgvault#new", Project: "msgvault", Title: input.Title, Body: input.Body, Revision: "1", List: input.List, Status: "open", State: personagenda.StateOpen, PriorityValue: input.PriorityValue}, nil
}
func (f *fakePersonAgendaOperations) Link(_ context.Context, _ int64, taskID, list string) (personagenda.Item, error) {
	f.linkedID = taskID
	return personagenda.Item{UID: "01LINKED", Ref: taskID, QualifiedRef: "msgvault#" + taskID, Project: "msgvault", Title: "Linked", Revision: "2", List: list, Status: "open", State: personagenda.StateOpen}, nil
}
func (f *fakePersonAgendaOperations) Unlink(_ context.Context, _ int64, taskID string) (personagenda.Item, error) {
	f.unlinked = taskID
	return personagenda.Item{UID: "01TASK", Ref: taskID, QualifiedRef: "msgvault#" + taskID, Project: "msgvault", Revision: "3", List: "agenda", Status: "open", State: personagenda.StateOpen}, nil
}
func (f *fakePersonAgendaOperations) Update(_ context.Context, _ int64, taskID string, input personagenda.UpdateInput) (personagenda.Item, error) {
	f.updated = input
	return personagenda.Item{UID: "01TASK", Ref: taskID, QualifiedRef: "msgvault#" + taskID, Project: "msgvault", Title: "Agenda item", Revision: "4", List: "agenda", Status: "open", State: personagenda.StateOpen}, nil
}

func TestPersonAgendaHTTP(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	operations := &fakePersonAgendaOperations{result: personagenda.Result{Project: "msgvault", PersonUID: "person-a", PersonUIDs: []string{"person-a", "retired-a"}, Truncated: true, Items: []personagenda.Item{{UID: "01ONE", Ref: "one", QualifiedRef: "msgvault#one", Project: "msgvault", Title: "Agenda item", Revision: "1", List: "agenda", Status: "open", State: personagenda.StateOpen}}}}
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), PersonAgendaOperations: operations})

	get := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/people/42/agenda", nil)
	server.router.ServeHTTP(get, request)
	require.Equal(http.StatusOK, get.Code, get.Body.String())
	var result PersonAgendaResult
	require.NoError(json.Unmarshal(get.Body.Bytes(), &result))
	require.Len(result.Items, 1)
	assert.Equal("msgvault", result.Project)
	assert.Equal("person-a", result.PersonUID)
	assert.Equal([]string{"person-a", "retired-a"}, result.PersonUIDs)
	assert.True(result.Truncated)
	assert.Equal("01ONE", result.Items[0].UID)
	assert.Equal("one", result.Items[0].Ref)
	assert.Equal("msgvault#one", result.Items[0].QualifiedRef)
	assert.Equal("Agenda item", result.Items[0].Title)

	createRequest := httptest.NewRequest(http.MethodPost, "/api/v1/people/42/agenda", strings.NewReader(`{"title":"Send notes","body":"Context","list":"follow up","priority":0}`))
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("Idempotency-Key", "request-1")
	created := httptest.NewRecorder()
	server.router.ServeHTTP(created, createRequest)
	require.Equal(http.StatusCreated, created.Code, created.Body.String())
	assert.Equal("Send notes", operations.created.Title)
	assert.Equal("Context", operations.created.Body)
	require.NotNil(operations.created.PriorityValue)
	assert.Equal(int64(0), *operations.created.PriorityValue)
	assert.Equal("request-1", operations.key)

	linked := personRequest(t, server, http.MethodPost, "/api/v1/people/42/agenda/links", []byte(`{"ref":"existing","list":"agenda"}`), "")
	require.Equal(http.StatusCreated, linked.Code, linked.Body.String())
	assert.Equal("existing", operations.linkedID)

	updated := personRequest(t, server, http.MethodPatch, "/api/v1/people/42/agenda/existing", []byte(`{"list":"gift ideas"}`), "")
	require.Equal(http.StatusOK, updated.Code, updated.Body.String())
	require.NotNil(operations.updated.List)
	assert.Equal("gift ideas", *operations.updated.List)

	unlinked := personRequest(t, server, http.MethodDelete, "/api/v1/people/42/agenda/existing", nil, "")
	require.Equal(http.StatusOK, unlinked.Code, unlinked.Body.String())
	assert.Equal("existing", operations.unlinked)
}

func TestDisabledPersonAgendaIsUnavailable(t *testing.T) {
	t.Parallel()
	backend := &personAgendaBackend{config: config.TaskIntegrationConfig{Enabled: false}}
	_, err := backend.List(t.Context(), 42)
	require.Error(t, err)

	response := httptest.NewRecorder()
	writePersonAgendaError(response, err)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
}

func TestPersonAgendaUpdateAcceptsOnlyListPlacement(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	operations := &fakePersonAgendaOperations{}
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), PersonAgendaOperations: operations})
	for _, body := range []string{`{"title":"Changed"}`, `{"body":"Changed"}`, `{"priority":1}`, `{"list":null}`, `{}`} {
		response := personRequest(t, server, http.MethodPatch, "/api/v1/people/42/agenda/task-1", []byte(body), "")
		assert.Equal(http.StatusBadRequest, response.Code, body)
	}
	response := personRequest(t, server, http.MethodPatch, "/api/v1/people/42/agenda/task-1", []byte(`{"list":"gift ideas"}`), "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	require.NotNil(operations.updated.List)
	assert.Equal("gift ideas", *operations.updated.List)
}

func TestPersonAgendaErrorResponses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"missing UID", personagenda.ErrPersonIdentity, http.StatusConflict, "person_identity_required"},
		{"lookup failure", personagenda.ErrIdentityLookup, http.StatusServiceUnavailable, "person_identity_unavailable"},
		{"oversized Kata response", taskclient.ErrResponseTooLarge, http.StatusServiceUnavailable, "task_response_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), PersonAgendaOperations: &fakePersonAgendaOperations{err: tc.err}})
			response := personRequest(t, server, http.MethodGet, "/api/v1/people/42/agenda", nil, "")
			require.Equal(t, tc.status, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), tc.code)
		})
	}
}

func TestPersonAgendaOpenAPIContract(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	document := OpenAPIDocument()
	agenda := document.Paths["/api/v1/people/{id}/agenda"]
	require.NotNil(agenda)
	assert.Equal("listPersonAgenda", agenda.Get.OperationID)
	assert.Equal("createPersonAgendaItem", agenda.Post.OperationID)
	var requestIDHeader *huma.Param
	for _, parameter := range agenda.Post.Parameters {
		if parameter.Name == "Idempotency-Key" {
			requestIDHeader = parameter
		}
	}
	require.NotNil(requestIDHeader)
	assert.Equal(headerParamLocation, requestIDHeader.In)
	assert.True(requestIDHeader.Required)
	for _, status := range []string{"401", "404", "409"} {
		assert.Contains(agenda.Get.Responses, status)
		assert.Contains(agenda.Post.Responses, status)
	}
	links := document.Paths["/api/v1/people/{id}/agenda/links"]
	require.NotNil(links)
	assert.Equal("linkPersonAgendaItem", links.Post.OperationID)
	unlink := document.Paths["/api/v1/people/{id}/agenda/{ref}"]
	require.NotNil(unlink)
	assert.Equal("updatePersonAgendaItem", unlink.Patch.OperationID)
	assert.Equal("unlinkPersonAgendaItem", unlink.Delete.OperationID)
	for _, status := range []string{"401", "404", "409"} {
		assert.Contains(unlink.Patch.Responses, status)
		assert.Contains(unlink.Delete.Responses, status)
	}
}
