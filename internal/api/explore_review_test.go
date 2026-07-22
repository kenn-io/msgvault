package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func TestExploreRelativeDateRevisionIgnoresParseClock(t *testing.T) {
	firstNow := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	secondNow := firstNow.Add(time.Minute)
	first := (&search.Parser{Now: func() time.Time { return firstNow }}).Parse("alpha newer_than:1d")
	second := (&search.Parser{Now: func() time.Time { return secondNow }}).Parse("alpha newer_than:1d")

	assert.Equal(t, canonicalParsedExploreQuery(first), canonicalParsedExploreQuery(second))
}

func TestExploreFullTextResolverBoundsCandidateTransfer(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)
	require.Equal(10_000, query.MaxExploreCandidateMessageIDs)
	store := &mockStore{stats: &StoreStats{}}
	store.searchMessagesQueryFunc = func(_ *search.Query, offset, limit int) ([]APIMessage, int64, error) {
		const resolverResultCount = 10_001
		remaining := resolverResultCount - offset
		if remaining <= 0 {
			return nil, resolverResultCount, nil
		}
		count := min(limit, remaining)
		messages := make([]APIMessage, count)
		for index := range messages {
			messages[index].ID = int64(offset + index + 1)
		}
		return messages, resolverResultCount, nil
	}
	base := newExploreDuckDBFixture(t)
	engine := &recordingExploreEngine{Engine: base, Explorer: base}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  store, Engine: engine, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"full_text","limit":50}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var body ExploreHTTPResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.True(body.CandidatePoolSaturated)
	assertions.Nil(body.TotalCount, "a bounded lexical candidate set must not publish an exact total")
	require.NotEmpty(store.searchMessagesQueryLimits)
	for _, limit := range store.searchMessagesQueryLimits {
		require.LessOrEqual(limit, exploreMaxLimit)
	}
	require.Equal(10_000, store.searchMessagesQueryTransferred)
	require.Len(engine.request.Search.CandidateMessageIDs, 10_000)
	preflight := postExploreJSON(t, srv, "/api/v1/explore/preflight", fmt.Sprintf(`{
		"selection":{"mode":"all_matching","predicate":{"query":"alpha","search_mode":"full_text"},
		"exclusions":[],"cache_revision":%q,"search_provenance":{"lexical_index_revision":%q}}
	}`, body.CacheRevision, body.SearchProvenance.LexicalIndexRevision))
	assertions.Equal(http.StatusConflict, preflight.Code, preflight.Body.String())
	assertions.Contains(preflight.Body.String(), "candidate_pool_saturated")
}

type recordingExploreEngine struct {
	query.Engine
	query.Explorer

	request query.ExploreRequest
}

func (e *recordingExploreEngine) Explore(ctx context.Context, request query.ExploreRequest) (*query.ExploreResponse, error) {
	e.request = request
	return e.Explorer.Explore(ctx, request)
}

func TestExploreHybridMatchCountsUseCompleteLexicalMembership(t *testing.T) {
	requirements := require.New(t)
	srv := newReviewSemanticServerWithHits(t, []vector.Hit{{MessageID: 1, Score: .9, Rank: 1}})
	request := ExploreHTTPRequest{Query: "alpha", SearchMode: exploreSearchModeHybrid}
	snapshotID := srv.exploreState.issueSnapshot(exploreCandidateSnapshot{
		RequestHash: exploreSnapshotRequestHash(request), IDs: []int64{1}, LexicalIDs: []int64{1, 2},
		Generation: 7, LexicalRevision: "fts5:exact-membership",
	})
	response := postExploreJSON(t, srv, "/api/v1/explore/match-counts", fmt.Sprintf(`{
		"predicate":{"query":"alpha","search_mode":"hybrid","candidate_snapshot_id":%q},
		"row_keys":["source:1:message:m2"]
	}`, snapshotID))
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body ExploreMatchCountsResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	requirements.Len(body.Counts, 1)
	assert.Equal(t, int64(1), body.Counts[0].Count)
}

func TestExploreSnapshotPreservesResolvedEmptyLexicalMembership(t *testing.T) {
	state := newExploreServerState(time.Now)
	request := ExploreHTTPRequest{Query: "alpha", SearchMode: exploreSearchModeHybrid}
	token := state.issueSnapshot(exploreCandidateSnapshot{
		RequestHash: exploreSnapshotRequestHash(request),
		IDs:         []int64{1},
		LexicalIDs:  make([]int64, 0),
	})

	snapshot, ok := state.snapshot(token, exploreSnapshotRequestHash(request))
	require.True(t, ok)
	assert.NotNil(t, snapshot.LexicalIDs, "resolved empty membership must remain distinct from unresolved nil membership")
	assert.Empty(t, snapshot.LexicalIDs)
}

func TestApplyIdentityScopeAcceptsCaseEquivalentDomain(t *testing.T) {
	context := query.Context{Domains: []string{"EXAMPLE.COM"}}
	err := applyIdentityScope(&context, ExploreFilter{Dimension: "domain", Values: []string{"example.com"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"example.com"}, context.Domains)
}

func TestApplyIdentityScopeNarrowsParticipantClusterToBasePredicate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	open := query.Context{}
	err := applyIdentityScope(&open, ExploreFilter{Dimension: "participant", Values: []string{"2", "3"}})
	require.NoError(err)
	assert.Equal([]int64{2, 3}, open.ParticipantIDs, "an open base predicate takes every cluster member")

	narrowed := query.Context{ParticipantIDs: []int64{2, 5}}
	err = applyIdentityScope(&narrowed, ExploreFilter{Dimension: "participant", Values: []string{"2", "3"}})
	require.NoError(err)
	assert.Equal([]int64{2}, narrowed.ParticipantIDs, "a base participant filter keeps only the members it allows")

	excluded := query.Context{ParticipantIDs: []int64{9}}
	err = applyIdentityScope(&excluded, ExploreFilter{Dimension: "participant", Values: []string{"2", "3"}})
	require.Error(err, "a base predicate excluding every member must fail loudly")
}

func TestExploreRejectsTamperedCursorsOnEveryPaginatedSurface(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		firstBody string
		field     string
		value     any
		server    func(*testing.T) *Server
	}{
		{
			name: "explore offset", path: "/api/v1/explore", firstBody: `{"limit":1}`,
			field: "offset", value: float64(499), server: func(t *testing.T) *Server {
				t.Helper()
				return newTestServerWithEngine(t, newExploreDuckDBFixture(t))
			},
		},
		{
			name: "groups cache revision", path: "/api/v1/explore/groups", firstBody: `{"grouping":["source"],"limit":1}`,
			field: "revision", value: "cache-forged", server: func(t *testing.T) *Server {
				t.Helper()
				return newTestServerWithEngine(t, newExploreDuckDBFixture(t))
			},
		},
		{
			name: "files search revision", path: "/api/v1/explore/files", firstBody: `{"predicate":{},"limit":1}`,
			field: "search_revision", value: "fts5:forged", server: func(t *testing.T) *Server {
				t.Helper()
				return newTestServerWithEngine(t, newExploreDuckDBFixture(t))
			},
		},
		{
			name: "semantic snapshot token", path: "/api/v1/explore", firstBody: `{"query":"alpha","search_mode":"semantic","limit":1}`,
			field: "snapshot", value: "forged-snapshot", server: newReviewSemanticServer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			srv := tt.server(t)
			first := postExploreJSON(t, srv, tt.path, tt.firstBody)
			requirements.Equal(http.StatusOK, first.Code, first.Body.String())
			cursor := nextExploreCursor(t, first)
			tampered := tamperExploreCursor(t, cursor, tt.field, tt.value)
			secondBody := addExploreCursor(t, tt.firstBody, tampered)
			second := postExploreJSON(t, srv, tt.path, secondBody)
			assertions.Equal(http.StatusBadRequest, second.Code, second.Body.String())
			assertions.Contains(second.Body.String(), "invalid_cursor")
		})
	}
}

func TestExploreGroupsValidatesCompletePredicateBeforeQuerying(t *testing.T) {
	srv := newTestServerWithEngine(t, newExploreDuckDBFixture(t))
	tests := []struct {
		name string
		body string
	}{
		{name: "query without mode", body: `{"grouping":["source"],"query":"alpha"}`},
		{name: "mode without query", body: `{"grouping":["source"],"search_mode":"full_text"}`},
		{name: "unsupported presentation", body: `{"grouping":["source"],"presentation":"timeline"}`},
		{name: "unsupported sort", body: `{"grouping":["source"],"sort":[{"field":"key","direction":"sideways"}]}`},
		{name: "unknown group", body: `{"grouping":["sql"]}`},
		{name: "null group", body: `{"grouping":null}`},
		{name: "unknown field", body: `{"grouping":["source"],"sql":"select 1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := postExploreJSON(t, srv, "/api/v1/explore/groups", tt.body)
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		})
	}
}

func TestExploreGroupsPaginatesCompletePopulation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv := newTestServerWithEngine(t, newExploreDuckDBFixture(t))
	first := postExploreJSON(t, srv, "/api/v1/explore/groups", `{"grouping":["source"],"limit":1}`)
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	var firstPage ExploreGroupsHTTPResponse
	requirements.NoError(json.Unmarshal(first.Body.Bytes(), &firstPage))
	requirements.Len(firstPage.Rows, 1)
	requirements.NotEmpty(firstPage.NextCursor)

	second := postExploreJSON(t, srv, "/api/v1/explore/groups", fmt.Sprintf(
		`{"grouping":["source"],"limit":1,"cursor":%q}`, firstPage.NextCursor,
	))
	requirements.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondPage ExploreGroupsHTTPResponse
	requirements.NoError(json.Unmarshal(second.Body.Bytes(), &secondPage))
	requirements.Len(secondPage.Rows, 1)
	assertions.NotEqual(firstPage.Rows[0].Key, secondPage.Rows[0].Key)
	assertions.Equal(firstPage.TotalCount, secondPage.TotalCount)
}

func TestExploreSemanticUsesStrongestConversationHitInsteadOfChronologicalAnchor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := newExploreSemanticChatDuckDBFixture(t)
	const strongest = int64(1)
	const anchor = int64(2)
	const email = int64(3)
	backend := &fakeVectorBackend{
		active: &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		searchHits: []vector.Hit{
			{MessageID: strongest, Score: .95, Rank: 1},
			{MessageID: email, Score: .8, Rank: 2},
			{MessageID: anchor, Score: .4, Rank: 3},
		},
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	store := &mockStore{messages: []APIMessage{
		{ID: strongest, Snippet: "strongest conversation excerpt"},
		{ID: email, Snippet: "email excerpt"},
		{ID: anchor, Snippet: "newest conversation excerpt"},
	}, total: 3, stats: &StoreStats{}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store, Engine: engine,
		HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"semantic","limit":10}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	requirements.Len(body.Rows, 2)
	assertions.Equal(query.EntryConversation, body.Rows[0].Kind)
	requirements.NotNil(body.Rows[0].AnchorMessageID)
	assertions.Equal(anchor, *body.Rows[0].AnchorMessageID, "row identity remains chronological")
	assertions.Equal("strongest conversation excerpt", body.Rows[0].Match.StrongestExcerpt)
	requirements.NotNil(body.Rows[0].Match.SemanticScore)
	assertions.InDelta(.95, *body.Rows[0].Match.SemanticScore, .0001)
	assertions.Equal(query.EntryEmail, body.Rows[1].Kind)
}

func TestExploreMapsEveryCommittedCacheFailureToStructuredUnavailable(t *testing.T) {
	tests := []struct {
		name      string
		readiness query.CacheReadiness
		mutate    func(*testing.T, string)
	}{
		{
			name: "missing root", readiness: query.CacheAbsent,
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.Rename(dir, dir+".missing"))
			},
		},
		{
			name: "missing state", readiness: query.CacheInterrupted,
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.Remove(query.CacheStatePath(dir)))
			},
		},
		{
			name: "malformed state", readiness: query.CacheInterrupted,
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.WriteFile(query.CacheStatePath(dir), []byte("{"), 0o600))
			},
		},
		{
			name: "interrupted state", readiness: query.CacheInterrupted,
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				updateExploreCacheState(t, dir, func(state *query.CacheSyncState) { state.LastSyncAt = time.Time{} })
			},
		},
		{
			name: "stale state", readiness: query.CacheStaleSchema,
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				updateExploreCacheState(t, dir, func(state *query.CacheSyncState) { state.SchemaVersion-- })
			},
		},
		{
			name: "drifted state", readiness: query.CacheDrifted,
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				path := filepath.Join(dir, "messages", "year=2026", "messages.parquet")
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.NoError(t, os.Chtimes(path, info.ModTime().Add(time.Hour), info.ModTime().Add(time.Hour)))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			engine, analyticsDir := newExploreDuckDBFixtureWithDir(t)
			srv := newTestServerWithEngine(t, engine)
			tt.mutate(t, analyticsDir)
			response := postExploreJSON(t, srv, "/api/v1/explore", `{}`)
			requirements.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
			var body struct {
				Error          string               `json:"error"`
				Readiness      query.CacheReadiness `json:"readiness"`
				RecoveryAction string               `json:"recovery_action"`
			}
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assertions.Equal("analytical_cache_unavailable", body.Error)
			assertions.Equal(tt.readiness, body.Readiness)
			assertions.NotEmpty(body.RecoveryAction)
		})
	}
}

func TestExploreCanonicalizesParsedFullTextQueryForRevisionAndCountCache(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := newExploreDuckDBFixture(t)
	store := &mockStore{
		messages: []APIMessage{{ID: 1}, {ID: 2}}, total: 2, stats: &StoreStats{},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store, Engine: engine, Logger: testLogger(),
	})
	pairs := [][2]string{
		{"alpha beta", "alpha   beta"},
		{"from:alice@example.com subject:invoice", "subject:invoice from:alice@example.com"},
		{"from:alice@example.com from:bob@example.com", "from:bob@example.com from:alice@example.com"},
	}
	for _, pair := range pairs {
		first := postExploreJSON(t, srv, "/api/v1/explore", fmt.Sprintf(
			`{"query":%q,"search_mode":"full_text"}`, pair[0],
		))
		requirements.Equal(http.StatusOK, first.Code, first.Body.String())
		second := postExploreJSON(t, srv, "/api/v1/explore", fmt.Sprintf(
			`{"query":%q,"search_mode":"full_text"}`, pair[1],
		))
		requirements.Equal(http.StatusOK, second.Code, second.Body.String())
		var firstBody, secondBody ExploreHTTPResponse
		requirements.NoError(json.Unmarshal(first.Body.Bytes(), &firstBody))
		requirements.NoError(json.Unmarshal(second.Body.Bytes(), &secondBody))
		assertions.Equal(firstBody.SearchProvenance.LexicalIndexRevision, secondBody.SearchProvenance.LexicalIndexRevision, pair)
	}

	bareQueries := []string{"alpha beta", "beta alpha", "alpha alpha beta", "alpha beta beta"}
	var bareRevision string
	for _, queryText := range bareQueries {
		response := postExploreJSON(t, srv, "/api/v1/explore", fmt.Sprintf(
			`{"query":%q,"search_mode":"full_text"}`, queryText,
		))
		requirements.Equal(http.StatusOK, response.Code, response.Body.String())
		var body ExploreHTTPResponse
		requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
		if bareRevision == "" {
			bareRevision = body.SearchProvenance.LexicalIndexRevision
		}
		assertions.Equal(bareRevision, body.SearchProvenance.LexicalIndexRevision, queryText)
	}

	for _, queryText := range bareQueries {
		response := postExploreJSON(t, srv, "/api/v1/explore/match-counts", fmt.Sprintf(`{
			"predicate":{"query":%q,"search_mode":"full_text"},
			"row_keys":["source:1:message:m1"]
		}`, queryText))
		requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	}
	srv.exploreState.mu.Lock()
	assertions.Len(srv.exploreState.matchCounts, 1, "equivalent parsed queries share one count-cache entry")
	srv.exploreState.mu.Unlock()
}

func TestExploreSemanticSnapshotExpiresAndReportsCandidatePoolSaturation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	hits := make([]vector.Hit, exploreMaxLimit)
	for i := range hits {
		hits[i] = vector.Hit{MessageID: int64(i%2 + 1), Score: .9, Rank: i + 1}
	}
	srv := newReviewSemanticServerWithHits(t, hits)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	srv.exploreState.now = func() time.Time { return now }

	first := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"semantic","limit":1}`)
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	var firstBody ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(first.Body.Bytes(), &firstBody))
	assertions.True(firstBody.CandidatePoolSaturated)
	requirements.NotEmpty(firstBody.NextCursor)

	now = now.Add(exploreCandidateSnapshotTTL + time.Second)
	second := postExploreJSON(t, srv, "/api/v1/explore", addExploreCursor(t,
		`{"query":"alpha","search_mode":"semantic","limit":1}`, firstBody.NextCursor,
	))
	assertions.Equal(http.StatusConflict, second.Code, second.Body.String())
	assertions.Contains(second.Body.String(), "candidate_snapshot_expired")
}

func TestExploreRequiresAuthenticatedCSRFSafeRemoteMutation(t *testing.T) {
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: testSessionAPIKey}},
		Store:  &mockStore{stats: &StoreStats{}}, Engine: newExploreDuckDBFixture(t), Logger: testLogger(),
	})

	unauthenticated := performSavedViewRequest(t, srv, http.MethodPost, "/api/v1/explore", []byte(`{}`), nil)
	assert.Equal(t, http.StatusUnauthorized, unauthenticated.Code, unauthenticated.Body.String())

	session := loginSavedViewSession(t, srv)
	missingCSRF := performSavedViewRequest(t, srv, http.MethodPost, "/api/v1/explore", []byte(`{}`), session.headers())
	assert.Equal(t, http.StatusForbidden, missingCSRF.Code, missingCSRF.Body.String())

	authorized := performSavedViewRequest(t, srv, http.MethodPost, "/api/v1/explore", []byte(`{}`), session.mutationHeaders())
	assert.Equal(t, http.StatusOK, authorized.Code, authorized.Body.String())
}

func TestExploreFullTextUsesRealSQLiteFTS5Candidates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "thread", "Thread")
	requirements.NoError(err)
	messages := []struct {
		sourceID string
		body     string
	}{
		{sourceID: "m1", body: "alpha uniquely matches this message"},
		{sourceID: "m2", body: "beta belongs to the other message"},
	}
	for i, message := range messages {
		messageID, err := st.UpsertMessage(&store.Message{
			ConversationID: conversationID, SourceID: source.ID, SourceMessageID: message.sourceID,
			MessageType: "email", Subject: sql.NullString{String: message.body, Valid: true}, SizeEstimate: 100,
		})
		requirements.NoError(err)
		requirements.Equal(int64(i+1), messageID, "fixture IDs must align with committed analytical facts")
		requirements.NoError(st.UpsertMessageBody(messageID, sql.NullString{String: message.body, Valid: true}, sql.NullString{}))
	}
	_, err = st.BackfillFTS(nil)
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: st,
		Engine: newExploreDuckDBFixture(t), Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"full_text"}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	requirements.Len(body.Rows, 1)
	requirements.NotNil(body.Rows[0].AnchorMessageID)
	assertions.Equal(int64(1), *body.Rows[0].AnchorMessageID)
	assertions.NotEmpty(body.SearchProvenance.LexicalIndexRevision)
}

func updateExploreCacheState(t *testing.T, analyticsDir string, update func(*query.CacheSyncState)) {
	t.Helper()
	data, err := os.ReadFile(query.CacheStatePath(analyticsDir))
	require.NoError(t, err)
	var state query.CacheSyncState
	require.NoError(t, json.Unmarshal(data, &state))
	update(&state)
	data, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(query.CacheStatePath(analyticsDir), data, 0o600))
}

func newExploreSemanticChatDuckDBFixture(t *testing.T) *query.DuckDBEngine {
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
			values: `(1::BIGINT, 1::BIGINT, 'chat-1', 700::BIGINT, '', 'strongest conversation excerpt', TIMESTAMP '2026-07-18 10:00:00', 100::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'imessage', 2026, 7),
				(2::BIGINT, 1::BIGINT, 'chat-2', 700::BIGINT, '', 'newest conversation excerpt', TIMESTAMP '2026-07-18 12:00:00', 100::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'imessage', 2026, 7),
				(3::BIGINT, 2::BIGINT, 'mail-1', 701::BIGINT, 'middle-ranked email', 'email excerpt', TIMESTAMP '2026-07-18 11:00:00', 100::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', 2026, 7)`,
		},
		{dir: "sources", file: "sources.parquet", columns: "id, account_email, source_type", values: `(1::BIGINT, '+15550000000', 'imessage'), (2::BIGINT, 'archive@example.com', 'gmail')`},
		{dir: "participants", file: "participants.parquet", columns: "id, email_address, domain, display_name, phone_number", values: `(1::BIGINT, 'alice@example.com', 'example.com', 'Alice', '')`, empty: true},
		{dir: "participant_identifiers", file: "participant_identifiers.parquet", columns: "participant_id, identifier_type, identifier_value, display_value, is_primary", values: `(1::BIGINT, 'email', 'alice@example.com', 'alice@example.com', true)`, empty: true},
		{dir: "message_recipients", file: "message_recipients.parquet", columns: "message_id, participant_id, recipient_type, display_name", values: `(1::BIGINT, 1::BIGINT, 'from', 'Alice')`, empty: true},
		{dir: "labels", file: "labels.parquet", columns: "id, name", values: `(0::BIGINT, '')`, empty: true},
		{dir: "message_labels", file: "message_labels.parquet", columns: "message_id, label_id", values: `(0::BIGINT, 0::BIGINT)`, empty: true},
		{dir: "attachments", file: "attachments.parquet", columns: "attachment_id, message_id, size, filename", values: `(0::BIGINT, 1::BIGINT, 0::BIGINT, '')`, empty: true},
		{dir: "conversations", file: "conversations.parquet", columns: "id, source_conversation_id, title, conversation_type", values: `(700::BIGINT, 'chat', 'Conversation', 'direct_chat'), (701::BIGINT, 'mail', '', 'email')`},
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
	return engine
}

func newReviewSemanticServer(t *testing.T) *Server {
	t.Helper()
	return newReviewSemanticServerWithHits(t, []vector.Hit{
		{MessageID: 1, Score: .9, Rank: 1}, {MessageID: 2, Score: .8, Rank: 2},
	})
}

func newReviewSemanticServerWithHits(t *testing.T, hits []vector.Hit) *Server {
	t.Helper()
	engine := newExploreDuckDBFixture(t)
	backend := &fakeVectorBackend{
		active:     &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		searchHits: hits,
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	store := &mockStore{messages: []APIMessage{{ID: 1}, {ID: 2}}, total: 2, stats: &StoreStats{}}
	return NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store, Engine: engine,
		HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})
}

func nextExploreCursor(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		NextCursor string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.NotEmpty(t, body.NextCursor)
	return body.NextCursor
}

func tamperExploreCursor(t *testing.T, encoded, field string, value any) string {
	t.Helper()
	payload := strings.SplitN(encoded, ".", 2)[0]
	data, err := base64.RawURLEncoding.DecodeString(payload)
	require.NoError(t, err)
	var cursor map[string]any
	require.NoError(t, json.Unmarshal(data, &cursor))
	cursor[field] = value
	data, err = json.Marshal(cursor)
	require.NoError(t, err)
	tamperedPayload := base64.RawURLEncoding.EncodeToString(data)
	if _, signature, found := strings.Cut(encoded, "."); found {
		return tamperedPayload + "." + signature
	}
	return tamperedPayload
}

func addExploreCursor(t *testing.T, body, cursor string) string {
	t.Helper()
	var object map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &object))
	object["cursor"] = cursor
	data, err := json.Marshal(object)
	require.NoError(t, err)
	return string(data)
}
