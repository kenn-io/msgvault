package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestResolveVisualPersonScopeUsesDurableBindingsAndLegacySenderAlias(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, catalog := newTestServerWithMockStore(t)
	catalog.personContextFunc = func(_ context.Context, id int64) (*store.Person, error) {
		return &store.Person{ID: id, ParticipantIDs: []int64{4, 9}}, nil
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/search/attachments/visual", nil)
	response := httptest.NewRecorder()
	query := visual.SearchQuery{}
	require.True(server.resolveVisualPersonScope(response, request, &query, 0, 0, 40, nil))
	require.NotNil(query.Person)
	assert.Equal([]int64{4, 9}, query.Person.ParticipantIDs)
	assert.Equal([]personscope.Direction{personscope.FromPerson}, query.Person.Directions)
}

func TestResolveVisualLegacySenderAliasReportsMissingDurablePerson(t *testing.T) {
	server, catalog := newTestServerWithMockStore(t)
	catalog.personContextFunc = func(context.Context, int64) (*store.Person, error) {
		return nil, store.ErrPersonNotFound
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/search/attachments/visual", nil)
	response := httptest.NewRecorder()

	ok := server.resolveVisualPersonScope(response, request, &visual.SearchQuery{}, 0, 0, 40, nil)

	assert.False(t, ok)
	assert.Equal(t, http.StatusNotFound, response.Code)
	assert.Contains(t, response.Body.String(), `"error":"person_not_found"`)
}

func TestResolveVisualPersonScopeRejectsInvalidDirectionBeforeResolution(t *testing.T) {
	server, catalog := newTestServerWithMockStore(t)
	catalog.personContextFunc = func(context.Context, int64) (*store.Person, error) {
		t.Fatal("invalid directions must not dispatch person resolution")
		return nil, store.ErrPersonNotFound
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/search/attachments/visual", nil)
	response := httptest.NewRecorder()

	ok := server.resolveVisualPersonScope(response, request, &visual.SearchQuery{}, 40, 0, 0,
		[]personscope.Direction{"sideways"})

	assert.False(t, ok)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Contains(t, response.Body.String(), `"error":"invalid_visual_person_scope"`)
}

func TestParseVisualSearchDatesPreservesRFC3339Precision(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	after := "2026-08-20T12:34:56.123456789-07:00"
	before := "2026-08-20T14:04:56.987654321-07:00"
	response := httptest.NewRecorder()
	query := visual.SearchQuery{}

	requirements.True(parseVisualSearchDates(response, after, before, &query))
	requirements.NotNil(query.After)
	requirements.NotNil(query.Before)
	assertions.Equal("2026-08-20T19:34:56.123456789Z", query.After.UTC().Format(time.RFC3339Nano))
	assertions.Equal("2026-08-20T21:04:56.987654321Z", query.Before.UTC().Format(time.RFC3339Nano))
}
