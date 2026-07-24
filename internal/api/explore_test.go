package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func TestExploreRejectsUnknownFilterDimension(t *testing.T) {
	srv := newTestServerWithEngine(t, newExploreDuckDBFixture(t))
	response := postExploreJSON(t, srv, "/api/v1/explore", `{"filters":[{"dimension":"sql","values":["select *"]}]}`)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Contains(t, response.Body.String(), "unknown filter dimension")
}

func TestExploreCursorAcceptsCanonicalFilterValueOrdering(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := newTestServerWithEngine(t, newExploreDuckDBFixture(t))
	first := postExploreJSON(t, srv, "/api/v1/explore", `{
		"filters":[{"dimension":"source","values":["2","1"]}],"limit":1
	}`)
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	var page ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(first.Body.Bytes(), &page))
	requirements.NotEmpty(page.NextCursor)

	second := postExploreJSON(t, srv, "/api/v1/explore", `{
		"filters":[{"dimension":"source","values":["1","2"]}],"limit":1,"cursor":"`+page.NextCursor+`"
	}`)
	requirements.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondPage ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(second.Body.Bytes(), &secondPage))
	requirements.Len(secondPage.Rows, 1)
	assertions.Equal("Older", secondPage.Rows[0].Title)
	assertions.Equal(page.CacheRevision, secondPage.CacheRevision)
}

func TestExploreUnavailableReturnsNamedReadinessAndRecovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, _ := newTestServerWithMockStore(t)
	response := postExploreJSON(t, srv, "/api/v1/explore", `{}`)
	requirements.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	var body ExploreCacheUnavailableResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.Equal("analytical_cache_unavailable", body.Error)
	assertions.Equal(query.CacheAbsent, body.Readiness)
	assertions.NotEmpty(body.RecoveryAction)
}

func TestExploreServerStateBoundsAndExpiresTransientCapabilities(t *testing.T) {
	assertions := assert.New(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	state := newExploreServerState(func() time.Time { return now })
	oldestOperationToken := state.issueOperation("oldest-selection", 1, "cache")
	var operationToken string
	for range exploreStateMaxEntries + 10 {
		now = now.Add(time.Millisecond)
		state.issueSnapshot(exploreCandidateSnapshot{RequestHash: "request", IDs: []int64{1}})
		operationToken = state.issueOperation("selection", 1, "cache")
		state.putMatchCounts(randomExploreToken(), map[string]int64{"row": 1})
	}
	state.mu.Lock()
	assertions.LessOrEqual(len(state.snapshots), exploreStateMaxEntries)
	assertions.LessOrEqual(len(state.operations), exploreStateMaxEntries)
	assertions.LessOrEqual(len(state.matchCounts), exploreStateMaxEntries)
	state.mu.Unlock()
	_, oldestOperationExists := state.operation(oldestOperationToken, "oldest-selection")
	assertions.False(oldestOperationExists, "the oldest operation grant is evicted at capacity")
	_, wrongSelectionExists := state.operation(operationToken, "other-selection")
	assertions.False(wrongSelectionExists, "operation grants stay bound to their preflight selection")
	grant, operationExists := state.operation(operationToken, "selection")
	assertions.True(operationExists)
	assertions.Equal(int64(1), grant.Count)
	assertions.Equal("cache", grant.Revision)

	now = now.Add(exploreCandidateSnapshotTTL + time.Second)
	state.issueSnapshot(exploreCandidateSnapshot{RequestHash: "fresh"})
	_, operationExists = state.operation(operationToken, "selection")
	assertions.False(operationExists)
	state.mu.Lock()
	assertions.Len(state.snapshots, 1)
	assertions.Empty(state.matchCounts)
	state.mu.Unlock()
}
