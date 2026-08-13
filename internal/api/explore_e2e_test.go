package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func TestExploreHTTPUsesCommittedDuckDBReadModel(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := newExploreDuckDBFixture(t)
	srv := newTestServerWithEngine(t, engine)

	response := postExploreJSON(t, srv, "/api/v1/explore", `{
		"filters":[{"dimension":"source","values":["1"]}],
		"presentation":"table",
		"sort":[{"field":"occurred_at","direction":"desc"}],
		"limit":1
	}`)

	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body map[string]any
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.InDelta(2, body["total_count"], 0)
	assertions.NotEmpty(body["cache_revision"])
	assertions.NotEmpty(body["next_cursor"])
	rows, ok := body["rows"].([]any)
	requirements.True(ok)
	requirements.Len(rows, 1)
	row, ok := rows[0].(map[string]any)
	requirements.True(ok)
	assertions.Equal("Newest", row["title"])
}

func TestExploreGroupsAndFilesUseCompleteDuckDBFacts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := newExploreDuckDBFixture(t)
	srv := newTestServerWithEngine(t, engine)

	groups := postExploreJSON(t, srv, "/api/v1/explore/groups", `{
		"grouping":["source"],"sort":[{"field":"count","direction":"desc"}],"limit":10
	}`)
	requirements.Equal(http.StatusOK, groups.Code, groups.Body.String())
	var groupBody struct {
		Rows []struct {
			Key   string `json:"key"`
			Count int64  `json:"count"`
		} `json:"rows"`
		TotalCount int64 `json:"total_count"`
	}
	requirements.NoError(json.Unmarshal(groups.Body.Bytes(), &groupBody))
	requirements.Len(groupBody.Rows, 2)
	assertions.Equal("1", groupBody.Rows[0].Key)
	assertions.Equal(int64(2), groupBody.Rows[0].Count)
	assertions.Equal(int64(2), groupBody.TotalCount)

	files := postExploreJSON(t, srv, "/api/v1/explore/files", `{"predicate":{"presentation":"table"},"limit":10}`)
	requirements.Equal(http.StatusOK, files.Code, files.Body.String())
	var fileBody struct {
		Files []query.ExploreFileFact `json:"files"`
	}
	requirements.NoError(json.Unmarshal(files.Body.Bytes(), &fileBody))
	requirements.Len(fileBody.Files, 2)
	assertions.Equal("newest.pdf", fileBody.Files[0].Filename)
	assertions.Equal("older.txt", fileBody.Files[1].Filename)
}

func TestExploreParticipantGroupsResolveDurableLabelsEndToEnd(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv := newTestServerWithEngine(t, newExploreDuckDBFixture(t))

	response := postExploreJSON(t, srv, "/api/v1/explore/groups", `{
		"grouping":["participant"],"sort":[{"field":"key","direction":"asc"}],"limit":10
	}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body struct {
		Rows []query.ExploreGroupRow `json:"rows"`
	}
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	requirements.Len(body.Rows, 2)
	assertions.Equal("1", body.Rows[0].Key)
	assertions.Equal("Alice", body.Rows[0].Label)
	assertions.Equal(int64(3), body.Rows[0].Count)
	assertions.Equal("2", body.Rows[1].Key)
	assertions.Equal("Bob", body.Rows[1].Label)
	assertions.Equal(int64(1), body.Rows[1].Count)
}

// TestExploreGroupsGroupKeyHydratesLowRankedGroupEndToEnd pins the wire
// mapping of the group_key request field: at limit 1 the default count-desc
// ranking returns Alice, so the keyed request resolving Bob proves the exact
// lookup reaches the engine instead of the ranked listing.
func TestExploreGroupsGroupKeyHydratesLowRankedGroupEndToEnd(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv := newTestServerWithEngine(t, newExploreDuckDBFixture(t))

	response := postExploreJSON(t, srv, "/api/v1/explore/groups", `{
		"grouping":["participant"],"group_key":"2","limit":1
	}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body struct {
		Rows       []query.ExploreGroupRow `json:"rows"`
		TotalCount int64                   `json:"total_count"`
		NextCursor string                  `json:"next_cursor"`
	}
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	requirements.Len(body.Rows, 1, "Alice outranks Bob; group_key must still resolve Bob at limit 1")
	assertions.Equal("2", body.Rows[0].Key)
	assertions.Equal("Bob", body.Rows[0].Label)
	assertions.Equal(int64(1), body.Rows[0].Count)
	assertions.Equal(int64(1), body.TotalCount)
	assertions.Empty(body.NextCursor)

	missing := postExploreJSON(t, srv, "/api/v1/explore/groups", `{
		"grouping":["participant"],"group_key":"999","limit":1
	}`)
	requirements.Equal(http.StatusOK, missing.Code, missing.Body.String())
	requirements.NoError(json.Unmarshal(missing.Body.Bytes(), &body))
	assertions.Empty(body.Rows)
	assertions.Equal(int64(0), body.TotalCount)
}

func TestExplorePreflightPinsRevisionAndExcludesCompletePredicate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := &rawExploreEngine{DuckDBEngine: newExploreDuckDBFixture(t)}
	srv := newTestServerWithEngine(t, engine)
	explore := postExploreJSON(t, srv, "/api/v1/explore", `{"filters":[{"dimension":"source","values":["1"]}],"limit":10}`)
	requirements.Equal(http.StatusOK, explore.Code, explore.Body.String())
	var explored struct {
		CacheRevision string `json:"cache_revision"`
	}
	requirements.NoError(json.Unmarshal(explore.Body.Bytes(), &explored))

	preflight := postExploreJSON(t, srv, "/api/v1/explore/preflight", fmt.Sprintf(`{
		"selection":{"mode":"all_matching","predicate":{"filters":[{"dimension":"source","values":["1"]}],"presentation":"table"},
		"exclusions":["source:1:message:m1"],"cache_revision":%q}
	}`, explored.CacheRevision))
	requirements.Equal(http.StatusOK, preflight.Code, preflight.Body.String())
	var body struct {
		Count              int64                      `json:"count"`
		EstimatedBytes     int64                      `json:"estimated_bytes"`
		OperationToken     string                     `json:"operation_token"`
		ActionTargets      []ExploreActionTarget      `json:"action_targets"`
		UnavailableActions []ExploreUnavailableAction `json:"unavailable_actions"`
	}
	requirements.NoError(json.Unmarshal(preflight.Body.Bytes(), &body))
	assertions.Equal(int64(1), body.Count)
	assertions.Equal(int64(220), body.EstimatedBytes)
	assertions.NotEmpty(body.OperationToken)
	assertions.Equal([]ExploreActionTarget{{Action: "export", MessageID: 2, Filename: "message-2.eml"}}, body.ActionTargets)
	assertions.Equal([]int64{2}, engine.rawRequests)
	assertions.Contains(body.UnavailableActions, ExploreUnavailableAction{
		Action: "open_in_source", Reason: "trusted_source_link_unavailable",
	})

	multi := postExploreJSON(t, srv, "/api/v1/explore/preflight", fmt.Sprintf(`{
		"selection":{"mode":"all_matching","predicate":{"filters":[{"dimension":"source","values":["1"]}]},"cache_revision":%q}
	}`, explored.CacheRevision))
	requirements.Equal(http.StatusOK, multi.Code, multi.Body.String())
	var multiBody ExplorePreflightResponse
	requirements.NoError(json.Unmarshal(multi.Body.Bytes(), &multiBody))
	assertions.Empty(multiBody.ActionTargets)
	assertions.Equal([]int64{2}, engine.rawRequests, "multi-row preflight must not request raw message data")
	assertions.Contains(multiBody.UnavailableActions, ExploreUnavailableAction{
		Action: "export", Reason: "browser_export_requires_single_message",
	})

	conflict := postExploreJSON(t, srv, "/api/v1/explore/preflight", `{
		"selection":{"mode":"all_matching","predicate":{"presentation":"table"},"cache_revision":"cache-old"}
	}`)
	assertions.Equal(http.StatusConflict, conflict.Code, conflict.Body.String())
	assertions.Contains(conflict.Body.String(), "archive_revision_changed")

	other := postExploreJSON(t, srv, "/api/v1/explore", `{"filters":[{"dimension":"source","values":["2"]}]}`)
	requirements.Equal(http.StatusOK, other.Code, other.Body.String())
	requirements.NoError(json.Unmarshal(other.Body.Bytes(), &explored))
	unavailable := postExploreJSON(t, srv, "/api/v1/explore/preflight", fmt.Sprintf(`{
		"selection":{"mode":"all_matching","predicate":{"filters":[{"dimension":"source","values":["2"]}]},"cache_revision":%q}
	}`, explored.CacheRevision))
	requirements.Equal(http.StatusOK, unavailable.Code, unavailable.Body.String())
	var unavailableBody ExplorePreflightResponse
	requirements.NoError(json.Unmarshal(unavailable.Body.Bytes(), &unavailableBody))
	assertions.Contains(unavailableBody.UnavailableActions, ExploreUnavailableAction{
		Action: "stage_deletion", Reason: "selection_contains_items_that_cannot_be_deleted_from_source",
	})
	assertions.Contains(unavailableBody.UnavailableActions, ExploreUnavailableAction{
		Action: "export_files", Reason: "selection_contains_no_files",
	})
	assertions.Contains(unavailableBody.UnavailableActions, ExploreUnavailableAction{
		Action: "export", Reason: "selection_has_no_exportable_raw_message",
	})
	assertions.Contains(unavailableBody.UnavailableActions, ExploreUnavailableAction{
		Action: "open_in_source", Reason: "trusted_source_link_unavailable",
	})
}

func TestExploreFullTextAndVisibleMatchCountsUseExactCandidates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := newExploreDuckDBFixture(t)
	store := &mockStore{
		messages: []APIMessage{{ID: 1, Subject: "Older", Snippet: "alpha match"}, {ID: 2, Subject: "Newest", Snippet: "alpha beta"}},
		total:    2, stats: &StoreStats{},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store, Engine: engine, Logger: testLogger(),
	})

	explore := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"full_text","limit":10}`)
	requirements.Equal(http.StatusOK, explore.Code, explore.Body.String())
	var explored struct {
		CacheRevision    string                 `json:"cache_revision"`
		SearchProvenance query.SearchProvenance `json:"search_provenance"`
	}
	requirements.NoError(json.Unmarshal(explore.Body.Bytes(), &explored))
	assertions.NotEmpty(explored.SearchProvenance.LexicalIndexRevision)

	counts := postExploreJSON(t, srv, "/api/v1/explore/match-counts", `{
		"predicate":{"query":"alpha","search_mode":"full_text"},
		"row_keys":["source:1:message:m1","source:1:message:m2"]
	}`)
	requirements.Equal(http.StatusOK, counts.Code, counts.Body.String())
	var body ExploreMatchCountsResponse
	requirements.NoError(json.Unmarshal(counts.Body.Bytes(), &body))
	assertions.Equal([]ExploreRowMatchCount{
		{RowKey: "source:1:message:m1", Count: 1},
		{RowKey: "source:1:message:m2", Count: 1},
	}, body.Counts)
	assertions.Equal(explored.CacheRevision, body.CacheRevision)
	assertions.Equal(explored.SearchProvenance.LexicalIndexRevision, body.LexicalRevision)
	assertions.NotEmpty(body.CanonicalQueryHash)
}

func TestExploreFullTextPaginationRejectsChangedLexicalRevision(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := newExploreDuckDBFixture(t)
	store := &mockStore{
		messages: []APIMessage{{ID: 1}, {ID: 2}},
		total:    2, stats: &StoreStats{},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store, Engine: engine, Logger: testLogger(),
	})

	first := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"full_text","limit":1}`)
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	var firstPage ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(first.Body.Bytes(), &firstPage))
	requirements.NotEmpty(firstPage.NextCursor)

	store.messages = []APIMessage{{ID: 2}}
	store.total = 1
	second := postExploreJSON(t, srv, "/api/v1/explore", fmt.Sprintf(
		`{"query":"alpha","search_mode":"full_text","limit":1,"cursor":%q}`, firstPage.NextCursor,
	))
	assertions.Equal(http.StatusConflict, second.Code, second.Body.String())
	assertions.Contains(second.Body.String(), "search_revision_changed")
}

func TestExploreSemanticIssuesBoundedSnapshotWithoutInventingTotal(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := newExploreDuckDBFixture(t)
	backend := &fakeVectorBackend{
		active:     &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		searchHits: []vector.Hit{{MessageID: 1, Score: .9, Rank: 1}, {MessageID: 2, Score: .8, Rank: 2}},
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	store := &mockStore{
		messages: []APIMessage{{ID: 1, Subject: "Older", Snippet: "older excerpt"}, {ID: 2, Subject: "Newest", Snippet: "newer excerpt"}},
		total:    2, stats: &StoreStats{},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store, Engine: engine,
		HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"semantic","limit":10}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var raw map[string]json.RawMessage
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &raw))
	assertions.NotContains(raw, "total_count")
	var body ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.NotEmpty(body.CandidateSnapshotID)
	requirements.NotNil(body.SearchProvenance.VectorGeneration)
	assertions.Equal(int64(7), *body.SearchProvenance.VectorGeneration)
	requirements.Len(body.Rows, 2)
	assertions.Equal(int64(1), *body.Rows[0].AnchorMessageID)
	assertions.Equal("older excerpt", body.Rows[0].Match.StrongestExcerpt)
	requirements.NotNil(body.Rows[0].Match.SemanticScore)
	assertions.InDelta(.9, *body.Rows[0].Match.SemanticScore, 0.0001)
	assertions.Equal(exploreMaxLimit, backend.searchLimit)
}

func TestExploreSemanticPaginationFollowsSnapshotRankNotArchiveDate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := newExploreDuckDBFixture(t)
	backend := &fakeVectorBackend{
		active:     &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		searchHits: []vector.Hit{{MessageID: 1, Score: .9, Rank: 1}, {MessageID: 2, Score: .8, Rank: 2}},
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	store := &mockStore{messages: []APIMessage{{ID: 1}, {ID: 2}}, total: 2, stats: &StoreStats{}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store, Engine: engine,
		HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})
	first := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"semantic","limit":1}`)
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	var firstPage ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(first.Body.Bytes(), &firstPage))
	requirements.Len(firstPage.Rows, 1)
	assertions.Equal(int64(1), *firstPage.Rows[0].AnchorMessageID)
	requirements.NotEmpty(firstPage.NextCursor)

	second := postExploreJSON(t, srv, "/api/v1/explore", fmt.Sprintf(`{"query":"alpha","search_mode":"semantic","limit":1,"cursor":%q}`, firstPage.NextCursor))
	requirements.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondPage ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(second.Body.Bytes(), &secondPage))
	requirements.Len(secondPage.Rows, 1)
	assertions.Equal(int64(2), *secondPage.Rows[0].AnchorMessageID)
}

// TestExploreSnapshotReuseRevalidatesActiveGeneration guards the cached
// snapshot path against generation swaps: a one-off scoped rebuild (or any
// full rebuild) can activate a new generation without changing the daemon's
// configured scope, so the status stays ready and only the snapshot pins
// the retired generation. Reuse must re-run the issuing path's
// active-generation check instead of serving retired candidates.
func TestExploreSnapshotReuseRevalidatesActiveGeneration(t *testing.T) {
	vecCfg := vector.Config{
		Enabled:    true,
		Embeddings: vector.EmbeddingsConfig{Model: "test", Dimension: 2},
	}
	fingerprint := vecCfg.GenerationFingerprint()
	newSnapshotServer := func(t *testing.T) (*Server, *fakeVectorBackend, string) {
		t.Helper()
		engine := newExploreDuckDBFixture(t)
		backend := &fakeVectorBackend{
			active:     &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: fingerprint, State: vector.GenerationActive},
			searchHits: []vector.Hit{{MessageID: 1, Score: .9, Rank: 1}, {MessageID: 2, Score: .8, Rank: 2}},
		}
		hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: fingerprint})
		store := &mockStore{messages: []APIMessage{{ID: 1}, {ID: 2}}, total: 2, stats: &StoreStats{}}
		srv := NewServerWithOptions(ServerOptions{
			Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store, Engine: engine,
			HybridEngine: hybridEngine, Backend: backend, VectorCfg: vecCfg, Logger: testLogger(),
		})
		first := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"semantic","limit":1}`)
		require.Equal(t, http.StatusOK, first.Code, first.Body.String())
		var firstPage ExploreHTTPResponse
		require.NoError(t, json.Unmarshal(first.Body.Bytes(), &firstPage))
		require.NotEmpty(t, firstPage.NextCursor)
		return srv, backend, firstPage.NextCursor
	}

	t.Run("same-fingerprint generation swap expires the snapshot", func(t *testing.T) {
		srv, backend, cursor := newSnapshotServer(t)
		backend.active = &vector.Generation{ID: 8, Model: "test", Dimension: 2, Fingerprint: fingerprint, State: vector.GenerationActive}

		second := postExploreJSON(t, srv, "/api/v1/explore", fmt.Sprintf(
			`{"query":"alpha","search_mode":"semantic","limit":1,"cursor":%q}`, cursor))
		assert.Equal(t, http.StatusConflict, second.Code, second.Body.String())
		assert.Contains(t, second.Body.String(), "candidate_snapshot_expired")
	})

	t.Run("fingerprint change returns index_stale", func(t *testing.T) {
		srv, backend, cursor := newSnapshotServer(t)
		backend.active = &vector.Generation{ID: 8, Model: "test", Dimension: 2, Fingerprint: fingerprint + ":ssrc-3", State: vector.GenerationActive}

		second := postExploreJSON(t, srv, "/api/v1/explore", fmt.Sprintf(
			`{"query":"alpha","search_mode":"semantic","limit":1,"cursor":%q}`, cursor))
		assert.Equal(t, http.StatusServiceUnavailable, second.Code, second.Body.String())
		assert.Contains(t, second.Body.String(), "index_stale")
	})

	t.Run("unchanged generation keeps serving the snapshot", func(t *testing.T) {
		srv, _, cursor := newSnapshotServer(t)
		second := postExploreJSON(t, srv, "/api/v1/explore", fmt.Sprintf(
			`{"query":"alpha","search_mode":"semantic","limit":1,"cursor":%q}`, cursor))
		assert.Equal(t, http.StatusOK, second.Code, second.Body.String())
	})
}

func TestExploreSemanticPreflightRequiresAndReusesCandidateSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := newExploreDuckDBFixture(t)
	backend := &fakeVectorBackend{
		active:     &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		searchHits: []vector.Hit{{MessageID: 1, Score: .9, Rank: 1}, {MessageID: 2, Score: .8, Rank: 2}},
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	store := &mockStore{messages: []APIMessage{{ID: 1}, {ID: 2}}, total: 2, stats: &StoreStats{}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store, Engine: engine,
		HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})
	explore := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"semantic","limit":10}`)
	requirements.Equal(http.StatusOK, explore.Code, explore.Body.String())
	var explored ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(explore.Body.Bytes(), &explored))

	preflight := postExploreJSON(t, srv, "/api/v1/explore/preflight", fmt.Sprintf(`{
		"selection":{"mode":"all_matching","predicate":{"query":"alpha","search_mode":"semantic","grouping":["source"]},
		"cache_revision":%q,"search_provenance":{"vector_generation":7},"candidate_snapshot_id":%q}
	}`, explored.CacheRevision, explored.CandidateSnapshotID))
	requirements.Equal(http.StatusOK, preflight.Code, preflight.Body.String())
	var body ExplorePreflightResponse
	requirements.NoError(json.Unmarshal(preflight.Body.Bytes(), &body))
	assertions.Equal(int64(2), body.Count)
	assertions.Equal(int64(330), body.EstimatedBytes)
	assertions.Equal(int64(7), *body.SearchProvenance.VectorGeneration)
}

func TestExploreIdentityFilterDirectionsAndHydrationAcrossSearchModes(t *testing.T) {
	fixture := newExploreIdentityAPIFixture(t)
	tests := []struct {
		name string
		body string
	}{
		{
			name: "base",
			body: exploreIdentityRequest("any", "", "", 10),
		},
		{
			name: "full text",
			body: exploreIdentityRequest("any", "alpha", exploreSearchModeFullText, 10),
		},
		{
			name: "semantic",
			body: exploreIdentityRequest("any", "alpha", exploreSearchModeSemantic, 10),
		},
		{
			name: "hybrid",
			body: exploreIdentityRequest("any", "alpha", exploreSearchModeHybrid, 10),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			callsBefore := fixture.store.matchCalls
			response := postExploreJSON(t, fixture.server, "/api/v1/explore", tt.body)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var body ExploreHTTPResponse
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			require.Len(body.Rows, 2)
			assert.Equal(callsBefore+1, fixture.store.matchCalls)
			assert.ElementsMatch([]int64{1, 2}, fixture.store.matchIDs[len(fixture.store.matchIDs)-1])

			rowsByMessage := make(map[int64]query.EntryRow, len(body.Rows))
			for _, row := range body.Rows {
				require.NotNil(row.AnchorMessageID)
				rowsByMessage[*row.AnchorMessageID] = row
				assert.NotNil(row.MatchedSenderIdentities)
				assert.NotNil(row.MatchedRecipientIdentities)
			}
			assert.Equal([]string{"Bob@Members.Example"}, rowsByMessage[1].MatchedSenderIdentities)
			assert.Empty(rowsByMessage[1].MatchedRecipientIdentities)
			assert.Empty(rowsByMessage[2].MatchedSenderIdentities)
			assert.Equal([]string{"Bob@Members.Example"}, rowsByMessage[2].MatchedRecipientIdentities)
			assert.Equal("Older", rowsByMessage[1].Title)
			assert.Equal([]string{"Bob"}, rowsByMessage[1].ParticipantLabels)
			assert.Equal("Newest", rowsByMessage[2].Title)
			assert.Equal([]string{"Alice", "Bob"}, rowsByMessage[2].ParticipantLabels)
			assert.NotContains(rowsByMessage, int64(3), "the shared participant address must not leak across sources")
		})
	}

	directions := []struct {
		direction string
		wantIDs   []int64
	}{
		{direction: "sender", wantIDs: []int64{1}},
		{direction: "recipient", wantIDs: []int64{2}},
		{direction: "any", wantIDs: []int64{1, 2}},
	}
	for _, tt := range directions {
		t.Run(tt.direction, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			response := postExploreJSON(t, fixture.server, "/api/v1/explore", exploreIdentityRequest(tt.direction, "", "", 10))
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var body ExploreHTTPResponse
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			gotIDs := make([]int64, 0, len(body.Rows))
			for _, row := range body.Rows {
				require.NotNil(row.AnchorMessageID)
				gotIDs = append(gotIDs, *row.AnchorMessageID)
			}
			assert.ElementsMatch(tt.wantIDs, gotIDs)
		})
	}
}

func TestExploreIdentityFilterCursorCanonicalizesEmptyDirectionAsAny(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newExploreIdentityAPIFixture(t)
	emptyDirection := postExploreJSON(t, fixture.server, "/api/v1/explore", exploreIdentityRequest("", "", "", 1))
	require.Equal(http.StatusOK, emptyDirection.Code, emptyDirection.Body.String())
	var emptyPage ExploreHTTPResponse
	require.NoError(json.Unmarshal(emptyDirection.Body.Bytes(), &emptyPage))
	require.NotEmpty(emptyPage.NextCursor)

	explicitAny := postExploreJSON(t, fixture.server, "/api/v1/explore", exploreIdentityRequest("any", "", "", 1))
	require.Equal(http.StatusOK, explicitAny.Code, explicitAny.Body.String())
	var anyPage ExploreHTTPResponse
	require.NoError(json.Unmarshal(explicitAny.Body.Bytes(), &anyPage))
	assert.Equal(emptyPage.NextCursor, anyPage.NextCursor)

	secondRequest := fmt.Sprintf(`{
		"filters":[
			{"dimension":"source","values":["1"]},
			{"dimension":"identity","values":["1","BOB@MEMBERS.EXAMPLE","any"]}
		],
		"limit":1,
		"cursor":%q
	}`, emptyPage.NextCursor)
	second := postExploreJSON(t, fixture.server, "/api/v1/explore", secondRequest)
	require.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondPage ExploreHTTPResponse
	require.NoError(json.Unmarshal(second.Body.Bytes(), &secondPage))
	require.Len(secondPage.Rows, 1)
	assert.NotNil(secondPage.Rows[0].MatchedSenderIdentities)
	assert.NotNil(secondPage.Rows[0].MatchedRecipientIdentities)
}

func TestExploreFilesIdentityCursorCanonicalizesEmptyDirectionAsAny(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newExploreIdentityAPIFixture(t)

	requestBody := func(direction string) string {
		return fmt.Sprintf(`{
			"predicate":{"filters":[
				{"dimension":"source","values":["1"]},
				{"dimension":"identity","values":["1","BOB@MEMBERS.EXAMPLE",%q]}
			]},
			"limit":1
		}`, direction)
	}
	emptyDirection := postExploreJSON(t, fixture.server, "/api/v1/explore/files", requestBody(""))
	requirements.Equal(http.StatusOK, emptyDirection.Code, emptyDirection.Body.String())
	var emptyPage ExploreFilesHTTPResponse
	requirements.NoError(json.Unmarshal(emptyDirection.Body.Bytes(), &emptyPage))
	requirements.NotEmpty(emptyPage.NextCursor)

	explicitAny := postExploreJSON(t, fixture.server, "/api/v1/explore/files", requestBody("any"))
	requirements.Equal(http.StatusOK, explicitAny.Code, explicitAny.Body.String())
	var anyPage ExploreFilesHTTPResponse
	requirements.NoError(json.Unmarshal(explicitAny.Body.Bytes(), &anyPage))
	assertions.Equal(emptyPage.NextCursor, anyPage.NextCursor)

	secondRequest := fmt.Sprintf(`{
		"predicate":{"filters":[
			{"dimension":"source","values":["1"]},
			{"dimension":"identity","values":["1","BOB@MEMBERS.EXAMPLE","any"]}
		]},
		"limit":1,
		"cursor":%q
	}`, emptyPage.NextCursor)
	second := postExploreJSON(t, fixture.server, "/api/v1/explore/files", secondRequest)
	requirements.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondPage ExploreFilesHTTPResponse
	requirements.NoError(json.Unmarshal(second.Body.Bytes(), &secondPage))
	requirements.Len(secondPage.Files, 1)
	assertions.Equal("older.txt", secondPage.Files[0].Filename)
}

func TestExploreIdentityFilterResolvesForGroupsFilesPreflightAndMatchCounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newExploreIdentityAPIFixture(t)
	identityFilters := `[
		{"dimension":"source","values":["1"]},
		{"dimension":"identity","values":["1","BOB@MEMBERS.EXAMPLE","any"]}
	]`

	groups := postExploreJSON(t, fixture.server, "/api/v1/explore/groups", fmt.Sprintf(`{
		"grouping":["source"],"filters":%s
	}`, identityFilters))
	require.Equal(http.StatusOK, groups.Code, groups.Body.String())
	var groupsBody ExploreGroupsHTTPResponse
	require.NoError(json.Unmarshal(groups.Body.Bytes(), &groupsBody))
	require.Len(groupsBody.Rows, 1)
	assert.Equal("1", groupsBody.Rows[0].Key)
	assert.Equal(int64(2), groupsBody.Rows[0].Count)

	files := postExploreJSON(t, fixture.server, "/api/v1/explore/files", fmt.Sprintf(`{
		"predicate":{"filters":%s},"limit":10
	}`, identityFilters))
	require.Equal(http.StatusOK, files.Code, files.Body.String())
	var filesBody ExploreFilesHTTPResponse
	require.NoError(json.Unmarshal(files.Body.Bytes(), &filesBody))
	assert.Equal(int64(2), filesBody.TotalCount)
	assert.Len(filesBody.Files, 2)

	explore := postExploreJSON(t, fixture.server, "/api/v1/explore", fmt.Sprintf(`{
		"filters":%s,"limit":10
	}`, identityFilters))
	require.Equal(http.StatusOK, explore.Code, explore.Body.String())
	var exploreBody ExploreHTTPResponse
	require.NoError(json.Unmarshal(explore.Body.Bytes(), &exploreBody))

	preflight := postExploreJSON(t, fixture.server, "/api/v1/explore/preflight", fmt.Sprintf(`{
		"selection":{
			"mode":"all_matching",
			"predicate":{"filters":%s},
			"cache_revision":%q
		}
	}`, identityFilters, exploreBody.CacheRevision))
	require.Equal(http.StatusOK, preflight.Code, preflight.Body.String())
	var preflightBody ExplorePreflightResponse
	require.NoError(json.Unmarshal(preflight.Body.Bytes(), &preflightBody))
	assert.Equal(int64(2), preflightBody.Count)

	matchCounts := postExploreJSON(t, fixture.server, "/api/v1/explore/match-counts", fmt.Sprintf(`{
		"predicate":{"query":"alpha","search_mode":"full_text","filters":%s},
		"row_keys":["source:1:message:m1","source:1:message:m2"]
	}`, identityFilters))
	require.Equal(http.StatusOK, matchCounts.Code, matchCounts.Body.String())
	var matchCountsBody ExploreMatchCountsResponse
	require.NoError(json.Unmarshal(matchCounts.Body.Bytes(), &matchCountsBody))
	assert.Equal([]ExploreRowMatchCount{
		{RowKey: "source:1:message:m1", Count: 1},
		{RowKey: "source:1:message:m2", Count: 1},
	}, matchCountsBody.Counts)
}

func TestExploreGroupsIdentityFilterPreservesTupleOrderAndNormalizesEmptyDirection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newExploreIdentityAPIFixture(t)
	response := postExploreJSON(t, fixture.server, "/api/v1/explore/groups", `{
		"grouping":["source"],
		"filters":[
			{"dimension":"source","values":["1"]},
			{"dimension":"identity","values":["1","bob@members.example",""]}
		]
	}`)

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var body ExploreGroupsHTTPResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(body.Rows, 1)
	assert.Equal("1", body.Rows[0].Key)
	assert.Equal(int64(2), body.Rows[0].Count)
}

func TestExploreHydrationUsesEmptyArraysForRowsWithoutAnchors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := newExploreDuckDBFixture(t)
	engine := &anchorlessExploreEngine{DuckDBEngine: base}
	srv := newTestServerWithEngine(t, engine)

	response := postExploreJSON(t, srv, "/api/v1/explore", `{"limit":1}`)

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var body ExploreHTTPResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(body.Rows, 1)
	assert.Nil(body.Rows[0].AnchorMessageID)
	assert.NotNil(body.Rows[0].MatchedSenderIdentities)
	assert.Empty(body.Rows[0].MatchedSenderIdentities)
	assert.NotNil(body.Rows[0].MatchedRecipientIdentities)
	assert.Empty(body.Rows[0].MatchedRecipientIdentities)
}

type anchorlessExploreEngine struct {
	*query.DuckDBEngine
}

func (e *anchorlessExploreEngine) Explore(ctx context.Context, request query.ExploreRequest) (*query.ExploreResponse, error) {
	result, err := e.DuckDBEngine.Explore(ctx, request)
	if err == nil && len(result.Rows) > 0 {
		result.Rows[0].AnchorMessageID = nil
	}
	return result, err
}

type recordingMessageIdentityStore struct {
	*store.Store

	resolveCalls int
	matchCalls   int
	matchIDs     [][]int64
}

func (s *recordingMessageIdentityStore) ResolveAccountIdentityContext(
	ctx context.Context,
	sourceID int64,
	identifier string,
) (store.ResolvedAccountIdentity, error) {
	s.resolveCalls++
	return s.Store.ResolveAccountIdentityContext(ctx, sourceID, identifier)
}

func (s *recordingMessageIdentityStore) MatchMessageIdentitiesContext(
	ctx context.Context,
	messageIDs []int64,
) (map[int64]store.MessageIdentityMatch, error) {
	s.matchCalls++
	s.matchIDs = append(s.matchIDs, append([]int64(nil), messageIDs...))
	return s.Store.MatchMessageIdentitiesContext(ctx, messageIDs)
}

type exploreIdentityAPIFixture struct {
	server *Server
	store  *recordingMessageIdentityStore
}

func newExploreIdentityAPIFixture(t *testing.T) exploreIdentityAPIFixture {
	t.Helper()
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	sourceA, err := st.GetOrCreateSource("gmail", "archive-a@example.com")
	requirements.NoError(err)
	sourceB, err := st.GetOrCreateSource("imap", "archive-b@example.com")
	requirements.NoError(err)
	requirements.Equal(int64(1), sourceA.ID)
	requirements.Equal(int64(2), sourceB.ID)
	conversationA, err := st.EnsureConversation(sourceA.ID, "thread-a", "Thread A")
	requirements.NoError(err)
	conversationB, err := st.EnsureConversation(sourceB.ID, "thread-b", "Thread B")
	requirements.NoError(err)
	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	requirements.NoError(err)
	bobID, err := st.EnsureParticipant("BOB@MEMBERS.EXAMPLE", "Bob", "members.example")
	requirements.NoError(err)
	requirements.Equal(int64(1), aliceID)
	requirements.Equal(int64(2), bobID)

	messages := []struct {
		sourceID       int64
		conversationID int64
		sourceMessage  string
		senderID       int64
		sentAt         time.Time
	}{
		{sourceA.ID, conversationA, "m1", bobID, time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)},
		{sourceA.ID, conversationA, "m2", aliceID, time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC)},
		{sourceB.ID, conversationB, "m3", aliceID, time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)},
	}
	for i, message := range messages {
		messageID, err := st.UpsertMessage(&store.Message{
			ConversationID: message.conversationID, SourceID: message.sourceID,
			SourceMessageID: message.sourceMessage, MessageType: "email",
			SentAt:   sql.NullTime{Time: message.sentAt, Valid: true},
			SenderID: sql.NullInt64{Int64: message.senderID, Valid: true},
			Subject:  sql.NullString{String: "alpha", Valid: true}, SizeEstimate: 100,
		})
		requirements.NoError(err)
		requirements.Equal(int64(i+1), messageID)
		requirements.NoError(st.UpsertMessageBody(messageID, sql.NullString{String: "alpha", Valid: true}, sql.NullString{}))
	}
	for i := range 10 {
		requirements.NoError(st.UpsertAttachment(
			3,
			fmt.Sprintf("fixture-padding-%d.bin", i),
			"application/octet-stream",
			"",
			fmt.Sprintf("fixture-padding-%d", i),
			1,
		))
	}
	requirements.NoError(st.UpsertAttachment(1, "older.txt", "text/plain", "", "", 10))
	requirements.NoError(st.UpsertAttachment(2, "newest.pdf", "application/pdf", "", "", 20))
	requirements.NoError(st.ReplaceMessageRecipients(1, "from", []int64{bobID}, []string{"Bob"}))
	requirements.NoError(st.ReplaceMessageRecipients(2, "from", []int64{aliceID}, []string{"Alice"}))
	requirements.NoError(st.ReplaceMessageRecipients(2, "to", []int64{bobID}, []string{"Bob"}))
	requirements.NoError(st.ReplaceMessageRecipients(3, "from", []int64{aliceID}, []string{"Alice"}))
	requirements.NoError(st.ReplaceMessageRecipients(3, "to", []int64{bobID}, []string{"Bob"}))
	requirements.NoError(st.AddAccountIdentity(sourceA.ID, "Bob@Members.Example", "manual"))
	requirements.NoError(st.AddAccountIdentity(sourceB.ID, "source-two@example.test", "manual"))
	_, err = st.BackfillFTS(nil)
	requirements.NoError(err)

	recipients := `(1::BIGINT, 2::BIGINT, 'from', 'Bob'),
		(2::BIGINT, 1::BIGINT, 'from', 'Alice'),
		(2::BIGINT, 2::BIGINT, 'to', 'Bob'),
		(3::BIGINT, 1::BIGINT, 'from', 'Alice'),
		(3::BIGINT, 2::BIGINT, 'to', 'Bob')`
	engine := newExploreDuckDBFixtureWithRecipients(t, recipients)
	backend := &fakeFusingBackend{
		fakeVectorBackend: &fakeVectorBackend{
			active: &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		},
		fusedHits: []vector.FusedHit{
			{MessageID: 1, RRFScore: .9, VectorScore: .9},
			{MessageID: 2, RRFScore: .8, VectorScore: .8},
			{MessageID: 3, RRFScore: .7, VectorScore: .7},
		},
	}
	backend.searchHits = []vector.Hit{
		{MessageID: 1, Score: .9, Rank: 1},
		{MessageID: 2, Score: .8, Rank: 2},
		{MessageID: 3, Score: .7, Rank: 3},
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	recordingStore := &recordingMessageIdentityStore{Store: st}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  recordingStore, Engine: engine, HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})
	return exploreIdentityAPIFixture{server: srv, store: recordingStore}
}

func exploreIdentityRequest(direction, queryText, searchMode string, limit int) string {
	search := ""
	if queryText != "" || searchMode != "" {
		search = fmt.Sprintf(`,"query":%q,"search_mode":%q`, queryText, searchMode)
	}
	return fmt.Sprintf(`{
		"filters":[
			{"dimension":"source","values":["1"]},
			{"dimension":"identity","values":["1","BOB@MEMBERS.EXAMPLE",%q]}
		],
		"limit":%d%s
	}`, direction, limit, search)
}

func postExploreJSON(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	return response
}

func newExploreDuckDBFixture(t *testing.T) *query.DuckDBEngine {
	t.Helper()
	engine, _ := newExploreDuckDBFixtureWithDir(t)
	return engine
}

type rawExploreEngine struct {
	*query.DuckDBEngine

	rawRequests []int64
}

func (e *rawExploreEngine) GetMessageRaw(_ context.Context, id int64) ([]byte, error) {
	e.rawRequests = append(e.rawRequests, id)
	if id == 1 || id == 2 {
		return []byte("From: archive-a@example.com\r\n\r\nraw"), nil
	}
	return nil, nil
}

// exploreFixtureDefaultMessages is the standard messages table for the
// explore DuckDB fixture: two live messages in source 1 and one in source 2.
const exploreFixtureDefaultMessages = `(1::BIGINT, 1::BIGINT, 'm1', 101::BIGINT, 'Older', 'alpha match', TIMESTAMP '2026-07-18 10:00:00', 100::BIGINT, true, 1::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', false, 2026, 7),
	(2::BIGINT, 1::BIGINT, 'm2', 102::BIGINT, 'Newest', 'alpha beta', TIMESTAMP '2026-07-18 11:00:00', 200::BIGINT, true, 1::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', false, 2026, 7),
	(3::BIGINT, 2::BIGINT, 'm3', 103::BIGINT, 'Other source', 'beta', TIMESTAMP '2026-07-18 09:00:00', 300::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', false, 2026, 7)`

const exploreFixtureDefaultRecipients = `(1::BIGINT, 1::BIGINT, 'from', 'Alice'), (2::BIGINT, 1::BIGINT, 'from', 'Alice'), (3::BIGINT, 1::BIGINT, 'from', 'Alice'), (3::BIGINT, 2::BIGINT, 'to', 'Bob')`

func newExploreDuckDBFixtureWithDir(t *testing.T) (*query.DuckDBEngine, string) {
	t.Helper()
	return newExploreDuckDBFixtureWithMessages(t, exploreFixtureDefaultMessages, 3)
}

// newExploreDuckDBFixtureWithMessages builds the explore Parquet fixture with
// a caller-supplied messages VALUES list (matching the standard column list
// below) so tests can vary message rows — e.g. mark one source-deleted —
// while sharing the surrounding sources/participants/attachments tables.
func newExploreDuckDBFixtureWithMessages(
	t *testing.T, messageValues string, lastMessageID int64,
) (*query.DuckDBEngine, string) {
	t.Helper()
	return newExploreDuckDBFixtureWithMessagesAndRecipients(
		t, messageValues, exploreFixtureDefaultRecipients, lastMessageID,
	)
}

func newExploreDuckDBFixtureWithRecipients(t *testing.T, recipientValues string) *query.DuckDBEngine {
	t.Helper()
	engine, _ := newExploreDuckDBFixtureWithMessagesAndRecipients(
		t, exploreFixtureDefaultMessages, recipientValues, 3,
	)
	return engine
}

func newExploreDuckDBFixtureWithMessagesAndRecipients(
	t *testing.T, messageValues, recipientValues string, lastMessageID int64,
) (*query.DuckDBEngine, string) {
	t.Helper()
	analyticsDir := t.TempDir()
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	tables := []struct {
		dir, file, columns, values string
		empty                      bool
	}{
		{
			dir: "messages/year=2026", file: "messages.parquet",
			columns: "id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, attachment_count, deleted_from_source_at, sender_id, message_type, is_from_me, year, month",
			values:  messageValues,
		},
		{dir: "sources", file: "sources.parquet", columns: "id, account_email, source_type", values: `(1::BIGINT, 'archive-a@example.com', 'gmail'), (2::BIGINT, 'archive-b@example.com', 'imap')`},
		{dir: "participants", file: "participants.parquet", columns: "id, email_address, domain, display_name, phone_number", values: `(1::BIGINT, 'alice@example.com', 'example.com', 'Alice', ''), (2::BIGINT, 'bob@members.example', 'members.example', 'Bob', '')`},
		{dir: "participant_identifiers", file: "participant_identifiers.parquet", columns: "participant_id, identifier_type, identifier_value, display_value, is_primary", values: `(1::BIGINT, 'email', 'alice@example.com', 'alice@example.com', true), (2::BIGINT, 'email', 'bob@members.example', 'bob@members.example', true)`},
		{dir: "message_recipients", file: "message_recipients.parquet", columns: "message_id, participant_id, recipient_type, display_name", values: recipientValues},
		{dir: "labels", file: "labels.parquet", columns: "id, name", values: `(0::BIGINT, '')`, empty: true},
		{dir: "message_labels", file: "message_labels.parquet", columns: "message_id, label_id", values: `(0::BIGINT, 0::BIGINT)`, empty: true},
		{dir: "attachments", file: "attachments.parquet", columns: "attachment_id, message_id, size, filename", values: `(11::BIGINT, 1::BIGINT, 10::BIGINT, 'older.txt'), (12::BIGINT, 2::BIGINT, 20::BIGINT, 'newest.pdf')`},
		{dir: "conversations", file: "conversations.parquet", columns: "id, source_conversation_id, title, conversation_type", values: `(101::BIGINT, 'c1', '', 'email'), (102::BIGINT, 'c2', '', 'email'), (103::BIGINT, 'c3', '', 'email')`},
		{dir: "conversation_participants", file: "conversation_participants.parquet", columns: "conversation_id, participant_id", values: `(0::BIGINT, 0::BIGINT)`, empty: true},
		{dir: "owner_participants", file: "owner_participants.parquet", columns: "source_id, participant_id", values: `(0::BIGINT, 0::BIGINT)`, empty: true},
		{dir: "participant_clusters", file: "participant_clusters.parquet", columns: "participant_id, canonical_id", values: `(0::BIGINT, 0::BIGINT)`, empty: true},
	}
	for _, table := range tables {
		dir := filepath.Join(analyticsDir, table.dir)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		where := ""
		if table.empty {
			where = " WHERE false"
		}
		path := filepath.ToSlash(filepath.Join(dir, table.file))
		_, err := db.Exec(fmt.Sprintf("COPY (SELECT * FROM (VALUES %s) AS t(%s)%s) TO '%s' (FORMAT PARQUET)", table.values, table.columns, where, path))
		require.NoError(t, err, "write %s", table.dir)
	}
	ensureIdentityCacheFixtureDatasets(t, db, analyticsDir)
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	require.NoError(t, err)
	state, err := json.Marshal(query.CacheSyncState{
		LastMessageID: lastMessageID, LastSyncAt: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
		SchemaVersion: query.CacheSchemaVersion, PublishedAt: time.Date(2026, 7, 18, 12, 1, 0, 0, time.UTC),
		DatasetFingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(query.CacheStatePath(analyticsDir), state, 0o600))

	engine, err := query.NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	return engine, analyticsDir
}
