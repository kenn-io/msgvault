package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func TestPersonTimelineReusesBaseSemanticSnapshotBeforeIdentityNarrowing(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := newReviewSemanticServerWithHits(t, []vector.Hit{
		{MessageID: 1, Score: .9, Rank: 1},
		{MessageID: 2, Score: .8, Rank: 2},
	})

	snapshot := mintIdentitySnapshot(t, srv, "/api/v1/people/search", `{
		"predicate":{"query":"alpha","search_mode":"semantic"},"limit":25
	}`)
	timeline := postExploreJSON(t, srv, "/api/v1/people/1/timeline", `{
		"query":"alpha","search_mode":"semantic","candidate_snapshot_id":"`+snapshot+`","limit":25
	}`)

	requirements.Equal(http.StatusOK, timeline.Code, timeline.Body.String())
	var body ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(timeline.Body.Bytes(), &body))
	assertions.Equal(snapshot, body.CandidateSnapshotID)
	requirements.NotNil(body.SearchProvenance.VectorGeneration)
	assertions.Equal(int64(7), *body.SearchProvenance.VectorGeneration)
	requirements.NotEmpty(body.Rows)
	for _, row := range body.Rows {
		requirements.NotNil(row.AnchorMessageID)
		assertions.Contains([]int64{1, 2}, *row.AnchorMessageID)
		assertions.Contains(row.ParticipantIDs, int64(1))
	}
}

func TestIdentityRelatedFilesReuseBaseSemanticSnapshotBeforeIdentityNarrowing(t *testing.T) {
	tests := []struct {
		name       string
		searchPath string
		filesPath  string
	}{
		{name: "person", searchPath: "/api/v1/people/search", filesPath: "/api/v1/people/1/files/search"},
		{name: "domain", searchPath: "/api/v1/domains/search", filesPath: "/api/v1/domains/example.com/files/search"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			srv := newReviewSemanticServerWithHits(t, []vector.Hit{
				{MessageID: 1, Score: .9, Rank: 1},
				{MessageID: 2, Score: .8, Rank: 2},
			})
			baseStore, ok := srv.store.(*mockStore)
			requirements.True(ok)
			srv.store = &fileCatalogStore{mockStore: baseStore, files: map[int64]store.FileMetadata{
				11: {ID: 11, MessageID: 1, ConversationID: 101, Filename: "older.txt"},
				12: {ID: 12, MessageID: 2, ConversationID: 102, Filename: "newest.pdf"},
			}}
			snapshot := mintIdentitySnapshot(t, srv, test.searchPath, `{
				"predicate":{"query":"alpha","search_mode":"semantic"},"limit":25
			}`)
			response := postExploreJSON(t, srv, test.filesPath, `{
				"predicate":{"query":"alpha","search_mode":"semantic","candidate_snapshot_id":"`+snapshot+`"},
				"sort":{"field":"occurred_at","direction":"desc"},"limit":25
			}`)

			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			var body FileSearchHTTPResponse
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assertions.Equal(snapshot, body.CandidateSnapshotID)
			requirements.NotNil(body.SearchProvenance.VectorGeneration)
			assertions.Equal(int64(7), *body.SearchProvenance.VectorGeneration)
			requirements.Len(body.Files, 2)
			assertions.ElementsMatch([]int64{11, 12}, []int64{body.Files[0].ID, body.Files[1].ID})
			for _, file := range body.Files {
				assertions.Contains(file.ParticipantIDs, int64(1))
				assertions.Contains(file.ParticipantDomains, "example.com")
			}
		})
	}
}

func TestIdentityScopeIntersectsSemanticCandidatesWithoutExpansion(t *testing.T) {
	srv := newIdentityScopeSemanticServer(t)
	for _, test := range []struct {
		name, searchPath, timelinePath, filesPath string
	}{
		{name: "person", searchPath: "/api/v1/people/search", timelinePath: "/api/v1/people/1/timeline", filesPath: "/api/v1/people/1/files/search"},
		{name: "domain", searchPath: "/api/v1/domains/search", timelinePath: "/api/v1/domains/example.com/timeline", filesPath: "/api/v1/domains/example.com/files/search"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			snapshot := mintIdentitySnapshot(t, srv, test.searchPath, `{
				"predicate":{"query":"alpha","search_mode":"semantic"},"limit":25
			}`)
			timeline := postExploreJSON(t, srv, test.timelinePath, `{
				"query":"alpha","search_mode":"semantic","candidate_snapshot_id":"`+snapshot+`"
			}`)
			requirements.Equal(http.StatusOK, timeline.Code, timeline.Body.String())
			var timelineBody ExploreHTTPResponse
			requirements.NoError(json.Unmarshal(timeline.Body.Bytes(), &timelineBody))
			requirements.Len(timelineBody.Rows, 1)
			requirements.NotNil(timelineBody.Rows[0].AnchorMessageID)
			assertions.Equal(int64(1), *timelineBody.Rows[0].AnchorMessageID)

			files := postExploreJSON(t, srv, test.filesPath, `{
				"predicate":{"query":"alpha","search_mode":"semantic","candidate_snapshot_id":"`+snapshot+`"},
				"sort":{"field":"occurred_at","direction":"desc"}
			}`)
			requirements.Equal(http.StatusOK, files.Code, files.Body.String())
			var filesBody FileSearchHTTPResponse
			requirements.NoError(json.Unmarshal(files.Body.Bytes(), &filesBody))
			requirements.Len(filesBody.Files, 1)
			assertions.Equal(int64(11), filesBody.Files[0].ID)
			assertions.Equal(snapshot, filesBody.CandidateSnapshotID)
			requirements.NotNil(filesBody.SearchProvenance.VectorGeneration)
			assertions.Equal(int64(7), *filesBody.SearchProvenance.VectorGeneration)
		})
	}
}

func TestIdentitySemanticSnapshotRejectsWrongPredicateAndExpiry(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	srv := newReviewSemanticServerWithHits(t, []vector.Hit{{MessageID: 1, Score: .9, Rank: 1}})
	baseStore, ok := srv.store.(*mockStore)
	require.True(t, ok)
	srv.store = &fileCatalogStore{mockStore: baseStore, files: map[int64]store.FileMetadata{
		11: {ID: 11, MessageID: 1, ConversationID: 101, Filename: "older.txt"},
	}}
	srv.exploreState = newExploreServerState(func() time.Time { return now })
	snapshot := mintIdentitySnapshot(t, srv, "/api/v1/people/search", `{
		"predicate":{"query":"alpha","search_mode":"semantic"},"limit":25
	}`)

	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{name: "timeline wrong query", path: "/api/v1/people/1/timeline", body: `{"query":"beta","search_mode":"semantic","candidate_snapshot_id":"` + snapshot + `"}`},
		{name: "timeline wrong mode", path: "/api/v1/people/1/timeline", body: `{"query":"alpha","search_mode":"hybrid","candidate_snapshot_id":"` + snapshot + `"}`},
		{name: "files wrong query", path: "/api/v1/people/1/files/search", body: `{"predicate":{"query":"beta","search_mode":"semantic","candidate_snapshot_id":"` + snapshot + `"},"sort":{"field":"occurred_at","direction":"desc"}}`},
		{name: "files wrong mode", path: "/api/v1/people/1/files/search", body: `{"predicate":{"query":"alpha","search_mode":"hybrid","candidate_snapshot_id":"` + snapshot + `"},"sort":{"field":"occurred_at","direction":"desc"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := postExploreJSON(t, srv, test.path, test.body)
			assertExploreError(t, response, http.StatusConflict, "candidate_snapshot_expired")
		})
	}

	now = now.Add(exploreCandidateSnapshotTTL + time.Second)
	expired := postExploreJSON(t, srv, "/api/v1/people/1/timeline", `{
		"query":"alpha","search_mode":"semantic","candidate_snapshot_id":"`+snapshot+`"
	}`)
	assertExploreError(t, expired, http.StatusConflict, "candidate_snapshot_expired")
	expiredFiles := postExploreJSON(t, srv, "/api/v1/people/1/files/search", `{
		"predicate":{"query":"alpha","search_mode":"semantic","candidate_snapshot_id":"`+snapshot+`"},
		"sort":{"field":"occurred_at","direction":"desc"}
	}`)
	assertExploreError(t, expiredFiles, http.StatusConflict, "candidate_snapshot_expired")
}

func TestIdentityScopeRejectsConflictingPredicateAndCrossIdentityCursor(t *testing.T) {
	requirements := require.New(t)
	srv := newReviewSemanticServerWithHits(t, []vector.Hit{
		{MessageID: 1, Score: .9, Rank: 1},
		{MessageID: 2, Score: .8, Rank: 2},
	})
	baseStore, ok := srv.store.(*mockStore)
	requirements.True(ok)
	srv.store = &fileCatalogStore{mockStore: baseStore, files: map[int64]store.FileMetadata{
		11: {ID: 11, MessageID: 1, ConversationID: 101, Filename: "older.txt"},
		12: {ID: 12, MessageID: 2, ConversationID: 102, Filename: "newest.pdf"},
	}}
	conflict := postExploreJSON(t, srv, "/api/v1/people/1/timeline", `{
		"filters":[{"dimension":"participant","values":["2"]}]
	}`)
	assertExploreError(t, conflict, http.StatusConflict, "identity_scope_conflict")

	snapshot := mintIdentitySnapshot(t, srv, "/api/v1/people/search", `{
		"predicate":{"query":"alpha","search_mode":"semantic"},"limit":25
	}`)
	first := postExploreJSON(t, srv, "/api/v1/people/1/timeline", `{
		"query":"alpha","search_mode":"semantic","candidate_snapshot_id":"`+snapshot+`","limit":1
	}`)
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	var page ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(first.Body.Bytes(), &page))
	requirements.NotEmpty(page.NextCursor)
	crossIdentity := postExploreJSON(t, srv, "/api/v1/people/2/timeline", `{
		"query":"alpha","search_mode":"semantic","candidate_snapshot_id":"`+snapshot+`","cursor":"`+page.NextCursor+`","limit":1
	}`)
	assertExploreError(t, crossIdentity, http.StatusBadRequest, "invalid_cursor")

	firstFiles := postExploreJSON(t, srv, "/api/v1/people/1/files/search", `{
		"predicate":{"query":"alpha","search_mode":"semantic","candidate_snapshot_id":"`+snapshot+`"},
		"sort":{"field":"occurred_at","direction":"desc"},"limit":1
	}`)
	requirements.Equal(http.StatusOK, firstFiles.Code, firstFiles.Body.String())
	var filePage FileSearchHTTPResponse
	requirements.NoError(json.Unmarshal(firstFiles.Body.Bytes(), &filePage))
	requirements.NotEmpty(filePage.NextCursor)
	crossIdentityFiles := postExploreJSON(t, srv, "/api/v1/people/2/files/search", `{
		"predicate":{"query":"alpha","search_mode":"semantic","candidate_snapshot_id":"`+snapshot+`"},
		"sort":{"field":"occurred_at","direction":"desc"},"cursor":"`+filePage.NextCursor+`","limit":1
	}`)
	assertExploreError(t, crossIdentityFiles, http.StatusBadRequest, "invalid_cursor")
}

func TestIdentityScopeNarrowsCompatibleMultiValueBaseFilter(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := newIdentityScopeSemanticServer(t)
	timeline := postExploreJSON(t, srv, "/api/v1/people/1/timeline", `{
		"filters":[{"dimension":"participant","values":["1","2"]}]
	}`)
	requirements.Equal(http.StatusOK, timeline.Code, timeline.Body.String())
	var body ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(timeline.Body.Bytes(), &body))
	requirements.Len(body.Rows, 1)
	requirements.NotNil(body.Rows[0].AnchorMessageID)
	assertions.Equal(int64(1), *body.Rows[0].AnchorMessageID)
}

func assertExploreError(t *testing.T, response interface {
	Result() *http.Response
}, status int, code string) {
	t.Helper()
	result := response.Result()
	defer func() { require.NoError(t, result.Body.Close()) }()
	require.Equal(t, status, result.StatusCode)
	var body ErrorResponse
	require.NoError(t, json.NewDecoder(result.Body).Decode(&body))
	assert.Equal(t, code, body.Error)
}

func mintIdentitySnapshot(t *testing.T, srv *Server, path, request string) string {
	t.Helper()
	response := postExploreJSON(t, srv, path, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var body struct {
		CandidateSnapshotID string `json:"candidate_snapshot_id"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.NotEmpty(t, body.CandidateSnapshotID)
	return body.CandidateSnapshotID
}

func newIdentityScopeSemanticServer(t *testing.T) *Server {
	t.Helper()
	engine := newIdentityScopeDuckDBFixture(t)
	backend := &fakeVectorBackend{
		active: &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		searchHits: []vector.Hit{
			{MessageID: 1, Score: .9, Rank: 1},
			{MessageID: 2, Score: .8, Rank: 2},
		},
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	baseStore := &mockStore{messages: []APIMessage{{ID: 1}, {ID: 2}}, total: 2, stats: &StoreStats{}}
	catalog := &fileCatalogStore{mockStore: baseStore, files: map[int64]store.FileMetadata{
		11: {ID: 11, MessageID: 1, ConversationID: 101, Filename: "alice.txt"},
		12: {ID: 12, MessageID: 2, ConversationID: 102, Filename: "bob.pdf"},
	}}
	return NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: catalog, Engine: engine,
		HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})
}

func newIdentityScopeDuckDBFixture(t *testing.T) *query.DuckDBEngine {
	t.Helper()
	analyticsDir := t.TempDir()
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	tables := []struct {
		dir, file, columns, values string
		empty                      bool
	}{
		{dir: "messages/year=2026", file: "messages.parquet", columns: "id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, attachment_count, deleted_from_source_at, sender_id, message_type, year, month", values: `(1::BIGINT, 1::BIGINT, 'm1', 101::BIGINT, 'Alice', 'alpha', TIMESTAMP '2026-07-18 10:00:00', 100::BIGINT, true, 1::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', 2026, 7), (2::BIGINT, 1::BIGINT, 'm2', 102::BIGINT, 'Bob', 'alpha', TIMESTAMP '2026-07-18 11:00:00', 200::BIGINT, true, 1::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', 2026, 7)`},
		{dir: "sources", file: "sources.parquet", columns: "id, account_email, source_type", values: `(1::BIGINT, 'archive@example.net', 'gmail')`},
		{dir: "participants", file: "participants.parquet", columns: "id, email_address, domain, display_name, phone_number", values: `(1::BIGINT, 'alice@example.com', 'example.com', 'Alice', ''), (2::BIGINT, 'bob@other.example', 'other.example', 'Bob', '')`},
		{dir: "participant_identifiers", file: "participant_identifiers.parquet", columns: "participant_id, identifier_type, identifier_value, display_value, is_primary", values: `(1::BIGINT, 'email', 'alice@example.com', 'alice@example.com', true), (2::BIGINT, 'email', 'bob@other.example', 'bob@other.example', true)`},
		{dir: "message_recipients", file: "message_recipients.parquet", columns: "message_id, participant_id, recipient_type, display_name", values: `(1::BIGINT, 1::BIGINT, 'from', 'Alice'), (2::BIGINT, 2::BIGINT, 'from', 'Bob')`},
		{dir: "labels", file: "labels.parquet", columns: "id, name", values: `(0::BIGINT, '')`, empty: true},
		{dir: "message_labels", file: "message_labels.parquet", columns: "message_id, label_id", values: `(0::BIGINT, 0::BIGINT)`, empty: true},
		{dir: "attachments", file: "attachments.parquet", columns: "attachment_id, message_id, size, filename", values: `(11::BIGINT, 1::BIGINT, 10::BIGINT, 'alice.txt'), (12::BIGINT, 2::BIGINT, 20::BIGINT, 'bob.pdf')`},
		{dir: "conversations", file: "conversations.parquet", columns: "id, source_conversation_id, title, conversation_type", values: `(101::BIGINT, 'c1', '', 'email'), (102::BIGINT, 'c2', '', 'email')`},
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
		LastMessageID: 2, LastSyncAt: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC), SchemaVersion: query.CacheSchemaVersion,
		PublishedAt: time.Date(2026, 7, 18, 12, 1, 0, 0, time.UTC), DatasetFingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(query.CacheStatePath(analyticsDir), state, 0o600))
	engine, err := query.NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	return engine
}
