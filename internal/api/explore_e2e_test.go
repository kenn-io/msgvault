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
	requirements.Len(body.Rows, 1)
	assertions.Equal("1", body.Rows[0].Key)
	assertions.Equal("Alice", body.Rows[0].Label)
	assertions.Equal(int64(3), body.Rows[0].Count)
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

func newExploreDuckDBFixtureWithDir(t *testing.T) (*query.DuckDBEngine, string) {
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
			columns: "id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, attachment_count, deleted_from_source_at, sender_id, message_type, year, month",
			values: `(1::BIGINT, 1::BIGINT, 'm1', 101::BIGINT, 'Older', 'alpha match', TIMESTAMP '2026-07-18 10:00:00', 100::BIGINT, true, 1::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', 2026, 7),
				(2::BIGINT, 1::BIGINT, 'm2', 102::BIGINT, 'Newest', 'alpha beta', TIMESTAMP '2026-07-18 11:00:00', 200::BIGINT, true, 1::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', 2026, 7),
				(3::BIGINT, 2::BIGINT, 'm3', 103::BIGINT, 'Other source', 'beta', TIMESTAMP '2026-07-18 09:00:00', 300::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', 2026, 7)`,
		},
		{dir: "sources", file: "sources.parquet", columns: "id, account_email, source_type", values: `(1::BIGINT, 'archive-a@example.com', 'gmail'), (2::BIGINT, 'archive-b@example.com', 'imap')`},
		{dir: "participants", file: "participants.parquet", columns: "id, email_address, domain, display_name, phone_number", values: `(1::BIGINT, 'alice@example.com', 'example.com', 'Alice', '')`},
		{dir: "participant_identifiers", file: "participant_identifiers.parquet", columns: "participant_id, identifier_type, identifier_value, display_value, is_primary", values: `(1::BIGINT, 'email', 'alice@example.com', 'alice@example.com', true)`},
		{dir: "message_recipients", file: "message_recipients.parquet", columns: "message_id, participant_id, recipient_type, display_name", values: `(1::BIGINT, 1::BIGINT, 'from', 'Alice'), (2::BIGINT, 1::BIGINT, 'from', 'Alice'), (3::BIGINT, 1::BIGINT, 'from', 'Alice')`},
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
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	require.NoError(t, err)
	state, err := json.Marshal(query.CacheSyncState{
		LastMessageID: 3, LastSyncAt: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
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
