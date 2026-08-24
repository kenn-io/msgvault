package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

type completionAPIStore struct {
	*mockStore

	rows    []store.PersonCompletion
	err     error
	members map[int64][]int64
}

func (s *completionAPIStore) CompletePersonProfilesContext(
	_ context.Context, _ store.PersonCompletionQuery,
) ([]store.PersonCompletion, error) {
	return s.rows, s.err
}

func (s *completionAPIStore) ClusterMembers(id int64) ([]int64, error) {
	return s.members[id], nil
}

func (s *completionAPIStore) ClusterEdges(int64) ([]store.LinkEdge, error) {
	return nil, nil
}

func TestParticipantCompletionMergesCanonicalTypedCandidatesWithoutURLQuery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := &peopleAPIEngine{
		MockEngine: &querytest.MockEngine{},
		completionResult: &query.PeopleCompletionResponse{
			CacheRevision: "cache-completion",
			Rows: []query.PeopleCompletion{
				{ParticipantID: 9, DisplayLabel: "Alice Observed", Kind: query.PeopleCompletionName,
					Value: "Alice", MatchValue: "alice", Source: "observed"},
				{ParticipantID: 9, DisplayLabel: "Alice Observed", Kind: query.PeopleCompletionUsername,
					Value: "@alice", MatchValue: "alice", Source: "slack"},
			},
		},
	}
	completionStore := &completionAPIStore{
		mockStore: &mockStore{}, members: map[int64][]int64{9: {7, 9}},
		rows: []store.PersonCompletion{
			{ParticipantID: 9, DisplayLabel: "Alice Profile", Kind: "name",
				Value: "Alice", MatchValue: "alice", Source: "nickname"},
			{ParticipantID: 9, DisplayLabel: "Alice Profile", Kind: "organization",
				Value: "Alice Industries", MatchValue: "alice industries", Source: "profile"},
		},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/participants/completions",
		bytes.NewBufferString(`{"query":"alice","limit":3}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newPeopleAPIServerWithStore(engine, completionStore).Router().ServeHTTP(response, request)

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Empty(request.URL.RawQuery)
	assert.Equal("no-store", response.Header().Get("Cache-Control"))
	assert.Equal(query.PeopleCompletionRequest{Query: "alice", Limit: 20}, engine.completionRequest)
	var body struct {
		Rows []struct {
			ParticipantID int64  `json:"participant_id"`
			DisplayLabel  string `json:"display_label"`
			Kind          string `json:"kind"`
			Value         string `json:"value"`
			Source        string `json:"source"`
			MatchValue    string `json:"match_value"`
		} `json:"rows"`
		CacheRevision string `json:"cache_revision"`
	}
	require.NoError(json.NewDecoder(response.Body).Decode(&body))
	assert.Equal("cache-completion", body.CacheRevision)
	require.Len(body.Rows, 3)
	assert.Equal(int64(7), body.Rows[0].ParticipantID)
	assert.Equal("Alice Profile", body.Rows[0].DisplayLabel)
	assert.Equal("name", body.Rows[0].Kind)
	assert.Equal("nickname", body.Rows[0].Source, "curated duplicate wins")
	assert.Equal("username", body.Rows[1].Kind)
	assert.Equal("organization", body.Rows[2].Kind)
	for _, row := range body.Rows {
		assert.Empty(row.MatchValue, "normalized values are internal")
	}
}

func TestParticipantCompletionMatchesAtPrefixedUsername(t *testing.T) {
	require := require.New(t)
	engine := &peopleAPIEngine{
		MockEngine: &querytest.MockEngine{},
		completionResult: &query.PeopleCompletionResponse{
			CacheRevision: "cache-completion",
			Rows: []query.PeopleCompletion{{
				ParticipantID: 9, DisplayLabel: "Alice", Kind: query.PeopleCompletionUsername,
				Value: "@alice", MatchValue: "alice", Source: "slack",
			}},
		},
	}
	completionStore := &completionAPIStore{mockStore: &mockStore{}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/participants/completions",
		bytes.NewBufferString(`{"query":"@alice"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newPeopleAPIServerWithStore(engine, completionStore).Router().ServeHTTP(response, request)

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var body ParticipantCompletionHTTPResponse
	require.NoError(json.NewDecoder(response.Body).Decode(&body))
	require.Len(body.Rows, 1)
	assert.Equal(t, ParticipantCompletionHTTPRow{
		ParticipantID: 9, DisplayLabel: "Alice", Kind: query.PeopleCompletionUsername,
		Value: "@alice", Source: "slack",
	}, body.Rows[0])
}

func TestParticipantCompletionCuratedDuplicatePreservesObservedRelationshipMetadata(t *testing.T) {
	when := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	server := newPeopleAPIServerWithStore(
		&peopleAPIEngine{MockEngine: &querytest.MockEngine{}},
		&completionAPIStore{mockStore: &mockStore{}},
	)

	rows, err := server.mergeParticipantCompletions("alice", []query.PeopleCompletion{{
		ParticipantID: 9, DisplayLabel: "Alice Observed", Kind: query.PeopleCompletionName,
		Value: "Alice", MatchValue: "alice", Source: "observed",
		CurrentRelationshipTemperature: 72, LastAt: when,
	}}, []store.PersonCompletion{{
		ParticipantID: 9, DisplayLabel: "Alice Profile", Kind: "name",
		Value: "Alice", MatchValue: "alice", Source: "nickname",
	}})

	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, query.PeopleCompletion{
		ParticipantID: 9, DisplayLabel: "Alice Profile", Kind: query.PeopleCompletionName,
		Value: "Alice", MatchValue: "alice", Source: "nickname",
		CurrentRelationshipTemperature: 72, LastAt: when,
	}, rows[0])
}

func TestParticipantCompletionValidatesBodyBeforeCallingBackends(t *testing.T) {
	tests := []string{
		`{"query":" ","limit":8}`,
		`{"query":"alice","limit":21}`,
		`{"query":"` + strings.Repeat("x", 257) + `","limit":8}`,
	}
	for _, body := range tests {
		t.Run(body[:min(len(body), 24)], func(t *testing.T) {
			engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{}}
			completionStore := &completionAPIStore{mockStore: &mockStore{}}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/participants/completions",
				bytes.NewBufferString(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			newPeopleAPIServerWithStore(engine, completionStore).Router().ServeHTTP(response, request)

			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assert.Empty(t, engine.completionRequest.Query)
		})
	}
}

func TestParticipantCompletionDoesNotReturnPartialRowsWhenProfileLookupFails(t *testing.T) {
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{},
		completionResult: &query.PeopleCompletionResponse{
			Rows: []query.PeopleCompletion{{ParticipantID: 1, Kind: query.PeopleCompletionName,
				Value: "Alice", MatchValue: "alice"}}, CacheRevision: "cache",
		}}
	completionStore := &completionAPIStore{mockStore: &mockStore{}, err: errors.New("profile unavailable")}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/participants/completions",
		bytes.NewBufferString(`{"query":"alice"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newPeopleAPIServerWithStore(engine, completionStore).Router().ServeHTTP(response, request)

	assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "Alice")
}
