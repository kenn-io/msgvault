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
	"slices"
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

// filteredCandidateMockSearch returns a SearchMessagesQuery stub over a fixed
// population split by source: unfiltered queries see every ID, while queries
// scoped to sourceID see only filteredIDs — mirroring how the real store
// applies AccountIDs inside SQLite before ranking and pagination.
func filteredCandidateMockSearch(allIDs, filteredIDs []int64, sourceID int64) func(*search.Query, int, int) ([]APIMessage, int64, error) {
	return func(q *search.Query, offset, limit int) ([]APIMessage, int64, error) {
		population := allIDs
		if slices.Equal(q.AccountIDs, []int64{sourceID}) {
			population = filteredIDs
		}
		total := int64(len(population))
		if offset >= len(population) {
			return nil, total, nil
		}
		end := min(offset+limit, len(population))
		messages := make([]APIMessage, 0, end-offset)
		for _, id := range population[offset:end] {
			messages = append(messages, APIMessage{ID: id})
		}
		return messages, total, nil
	}
}

func TestExploreFullTextAppliesFiltersBeforeCandidateCap(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	allIDs := make([]int64, query.MaxExploreCandidateMessageIDs+50)
	for i := range allIDs {
		allIDs[i] = int64(i + 1)
	}
	filteredIDs := allIDs[query.MaxExploreCandidateMessageIDs:]
	store := &mockStore{stats: &StoreStats{}}
	store.searchMessagesQueryFunc = filteredCandidateMockSearch(allIDs, filteredIDs, 2)
	base := newExploreDuckDBFixture(t)
	engine := &recordingExploreEngine{Engine: base, Explorer: base}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  store, Engine: engine, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/explore", `{
		"query":"alpha","search_mode":"full_text","limit":50,
		"filters":[{"dimension":"source","values":["2"]}]
	}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	requirements.NotNil(store.searchMessagesQueryLast)
	assertions.Equal([]int64{2}, store.searchMessagesQueryLast.AccountIDs, "the source filter must reach the lexical resolver")
	assertions.False(body.CandidatePoolSaturated, "the filtered population fits the candidate cap")
	assertions.NotNil(body.TotalCount, "an unsaturated candidate pool publishes an exact total")
	assertions.Equal(filteredIDs, engine.request.Search.CandidateMessageIDs, "candidates beyond the unfiltered cap must survive")

	groups := postExploreJSON(t, srv, "/api/v1/explore/groups", `{
		"grouping":["source"],"query":"alpha","search_mode":"full_text",
		"filters":[{"dimension":"source","values":["2"]}]
	}`)
	assertions.Equal(http.StatusOK, groups.Code, groups.Body.String())
}

func TestExploreHybridLexicalBranchResolvesWithFilters(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	allIDs := make([]int64, query.MaxExploreCandidateMessageIDs+1)
	for i := range allIDs {
		allIDs[i] = int64(i + 1)
	}
	backend := &fakeFusingBackend{
		fakeVectorBackend: &fakeVectorBackend{
			active: &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		},
		fusedHits: []vector.FusedHit{{MessageID: 1, RRFScore: .9, VectorScore: .9}},
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	store := &mockStore{stats: &StoreStats{}}
	store.searchMessagesQueryFunc = filteredCandidateMockSearch(allIDs, []int64{1, 2}, 1)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  store, Engine: newExploreDuckDBFixture(t),
		HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/explore", `{
		"query":"alpha","search_mode":"hybrid","limit":10,
		"filters":[{"dimension":"source","values":["1"]}]
	}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	requirements.NotNil(store.searchMessagesQueryLast)
	assertions.Equal([]int64{1}, store.searchMessagesQueryLast.AccountIDs, "the hybrid lexical branch must resolve with filters")
	assertions.False(body.CandidatePoolSaturated, "saturation reflects the filtered lexical membership")
	requirements.NotEmpty(body.CandidateSnapshotID)
	snapshot, ok := srv.exploreState.snapshot(body.CandidateSnapshotID, exploreSnapshotRequestHash(ExploreHTTPRequest{
		Query: "alpha", SearchMode: exploreSearchModeHybrid,
		Filters: []ExploreFilter{{Dimension: "source", Values: []string{"1"}}},
	}))
	requirements.True(ok)
	assertions.Equal([]int64{1, 2}, snapshot.LexicalIDs, "the snapshot stores the filtered lexical membership")
}

// fakeFusingBackend upgrades fakeVectorBackend with the FusingBackend
// capability so hybrid-mode explore requests can run end to end.
type fakeFusingBackend struct {
	*fakeVectorBackend

	fusedHits  []vector.FusedHit
	fusedCalls int
}

func (f *fakeFusingBackend) FusedSearch(_ context.Context, _ vector.FusedRequest) ([]vector.FusedHit, bool, error) {
	f.fusedCalls++
	return f.fusedHits, false, nil
}

func TestExploreFullTextSourceFilterFindsMatchesBeyondUnfilteredCap(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	sourceA, err := st.GetOrCreateSource("gmail", "archive-a@example.com")
	requirements.NoError(err)
	sourceB, err := st.GetOrCreateSource("imap", "archive-b@example.com")
	requirements.NoError(err)
	conversationA, err := st.EnsureConversation(sourceA.ID, "thread-a", "Thread A")
	requirements.NoError(err)
	conversationB, err := st.EnsureConversation(sourceB.ID, "thread-b", "Thread B")
	requirements.NoError(err)
	messages := []struct {
		sourceID       int64
		conversationID int64
		sourceMessage  string
		sentAt         time.Time
	}{
		{sourceA.ID, conversationA, "m1", time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)},
		{sourceA.ID, conversationA, "m2", time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC)},
		{sourceB.ID, conversationB, "m3", time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)},
	}
	const sharedBody = "alpha shared message body"
	for i, message := range messages {
		messageID, err := st.UpsertMessage(&store.Message{
			ConversationID: message.conversationID, SourceID: message.sourceID,
			SourceMessageID: message.sourceMessage, MessageType: "email",
			SentAt:  sql.NullTime{Time: message.sentAt, Valid: true},
			Subject: sql.NullString{String: sharedBody, Valid: true}, SizeEstimate: 100,
		})
		requirements.NoError(err)
		requirements.Equal(int64(i+1), messageID, "fixture IDs must align with committed analytical facts")
		requirements.NoError(st.UpsertMessageBody(messageID, sql.NullString{String: sharedBody, Valid: true}, sql.NullString{}))
	}
	_, err = st.BackfillFTS(nil)
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: st,
		Engine: newExploreDuckDBFixture(t), Logger: testLogger(),
	})
	srv.lexicalCandidateCap = 2

	unfiltered := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"full_text"}`)
	requirements.Equal(http.StatusOK, unfiltered.Code, unfiltered.Body.String())
	var unfilteredBody ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(unfiltered.Body.Bytes(), &unfilteredBody))
	requirements.True(unfilteredBody.CandidatePoolSaturated, "the shrunken cap must bind for the unfiltered query")

	filtered := postExploreJSON(t, srv, "/api/v1/explore", `{
		"query":"alpha","search_mode":"full_text",
		"filters":[{"dimension":"source","values":["2"]}]
	}`)
	requirements.Equal(http.StatusOK, filtered.Code, filtered.Body.String())
	var body ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(filtered.Body.Bytes(), &body))
	assertions.False(body.CandidatePoolSaturated, "the filtered population fits the cap")
	requirements.Len(body.Rows, 1, "the filtered match ranked beyond the unfiltered cap must be found")
	requirements.NotNil(body.Rows[0].AnchorMessageID)
	assertions.Equal(int64(3), *body.Rows[0].AnchorMessageID)
	requirements.NotNil(body.TotalCount)
	assertions.Equal(int64(1), *body.TotalCount)
}

func TestApplyLexicalFilterPushdown(t *testing.T) {
	afterFilter := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	beforeFilter := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	afterQuery := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	beforeQuery := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		parsed    search.Query
		filters   query.Context
		matchable bool
		want      search.Query
	}{
		{
			name:      "source filter fills empty accounts",
			filters:   query.Context{SourceIDs: []int64{2, 3}},
			matchable: true,
			want:      search.Query{AccountIDs: []int64{2, 3}},
		},
		{
			name:      "source filter intersects in operator",
			parsed:    search.Query{AccountIDs: []int64{1, 2}},
			filters:   query.Context{SourceIDs: []int64{2, 3}},
			matchable: true,
			want:      search.Query{AccountIDs: []int64{2}},
		},
		{
			name:      "disjoint sources match nothing",
			parsed:    search.Query{AccountIDs: []int64{1}},
			filters:   query.Context{SourceIDs: []int64{2}},
			matchable: false,
			want:      search.Query{AccountIDs: []int64{}},
		},
		{
			name:      "message type filter lowercases and intersects",
			parsed:    search.Query{MessageTypes: []string{"sms", "mms"}},
			filters:   query.Context{MessageTypes: []string{"SMS"}},
			matchable: true,
			want:      search.Query{MessageTypes: []string{"sms"}},
		},
		{
			name:      "disjoint message types match nothing",
			parsed:    search.Query{MessageTypes: []string{"email"}},
			filters:   query.Context{MessageTypes: []string{"sms"}},
			matchable: false,
			want:      search.Query{MessageTypes: []string{"email"}},
		},
		{
			name:      "date filters only tighten bounds",
			parsed:    search.Query{AfterDate: &afterQuery, BeforeDate: &beforeQuery},
			filters:   query.Context{After: &afterFilter, Before: &beforeFilter},
			matchable: true,
			want:      search.Query{AfterDate: &afterQuery, BeforeDate: &beforeQuery},
		},
		{
			name:      "date filters narrow open bounds",
			filters:   query.Context{After: &afterFilter, Before: &beforeFilter},
			matchable: true,
			want:      search.Query{AfterDate: &afterFilter, BeforeDate: &beforeFilter},
		},
		{
			name:      "participant and domain filters stay analytical",
			filters:   query.Context{ParticipantIDs: []int64{5}, Domains: []string{"example.com"}, Deletion: query.DeletionDeleted},
			matchable: true,
			want:      search.Query{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := tt.parsed
			matchable := applyLexicalFilterPushdown(&parsed, tt.filters)
			assert.Equal(t, tt.matchable, matchable)
			if matchable {
				assert.Equal(t, tt.want, parsed)
			}
		})
	}
}

// TestExploreSemanticVectorFilterMergeMirrorsLexicalPushdown proves the
// vector backend filter obeys the lexical pushdown semantics: request
// filters intersect with equivalent query operators and date bounds only
// tighten, so a filter can never broaden the candidate predicate beyond
// what the query text alone would match.
func TestExploreSemanticVectorFilterMergeMirrorsLexicalPushdown(t *testing.T) {
	utcDate := func(month, day int) *time.Time {
		bound := time.Date(2026, time.Month(month), day, 0, 0, 0, 0, time.UTC)
		return &bound
	}
	tests := []struct {
		name             string
		body             string
		wantSourceIDs    []int64
		wantMessageTypes []string
		wantAfter        *time.Time
		wantBefore       *time.Time
	}{
		{
			name: "filters fill open query dimensions",
			body: `{"query":"alpha","search_mode":"semantic","filters":[
				{"dimension":"source","values":["2"]},
				{"dimension":"message_type","values":["email"]}]}`,
			wantSourceIDs:    []int64{2},
			wantMessageTypes: []string{"email"},
		},
		{
			name: "message type filter intersects query operator instead of appending",
			body: `{"query":"alpha message_type:sms","search_mode":"semantic","filters":[
				{"dimension":"message_type","values":["SMS","email"]}]}`,
			wantMessageTypes: []string{"sms"},
		},
		{
			name: "tighter query date bounds survive looser filter bounds",
			body: `{"query":"alpha after:2026-03-01 before:2026-05-01","search_mode":"semantic","filters":[
				{"dimension":"after","values":["2026-01-01T00:00:00Z"]},
				{"dimension":"before","values":["2026-07-01T00:00:00Z"]}]}`,
			wantAfter:  utcDate(3, 1),
			wantBefore: utcDate(5, 1),
		},
		{
			name: "filter dates tighten open query bounds",
			body: `{"query":"alpha","search_mode":"semantic","filters":[
				{"dimension":"after","values":["2026-01-01T00:00:00Z"]}]}`,
			wantAfter: utcDate(1, 1),
		},
	}
	assertBound := func(t *testing.T, want, got *time.Time, bound string) {
		t.Helper()
		if want == nil {
			assert.Nil(t, got, bound)
			return
		}
		require.NotNil(t, got, bound)
		assert.True(t, want.Equal(*got), "%s: want %s, got %s", bound, want, got)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			backend := &fakeVectorBackend{
				active:     &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
				searchHits: []vector.Hit{{MessageID: 1, Score: .9, Rank: 1}},
			}
			hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
			store := &mockStore{messages: []APIMessage{{ID: 1}}, total: 1, stats: &StoreStats{}}
			srv := NewServerWithOptions(ServerOptions{
				Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store,
				Engine: newExploreDuckDBFixture(t), HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
			})

			response := postExploreJSON(t, srv, "/api/v1/explore", tt.body)
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			assertions.Equal(tt.wantSourceIDs, backend.searchFilter.SourceIDs, "SourceIDs")
			assertions.Equal(tt.wantMessageTypes, backend.searchFilter.MessageTypes, "MessageTypes")
			assertBound(t, tt.wantAfter, backend.searchFilter.After, "After")
			assertBound(t, tt.wantBefore, backend.searchFilter.Before, "Before")
		})
	}
}

func TestExploreSemanticDisjointFilterShortCircuitsToEmptyCandidates(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := &fakeVectorBackend{
		active:     &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		searchHits: []vector.Hit{{MessageID: 3, Score: .9, Rank: 1}},
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	store := &mockStore{messages: []APIMessage{{ID: 3}}, total: 1, stats: &StoreStats{}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store,
		Engine: newExploreDuckDBFixture(t), HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/explore", `{
		"query":"alpha message_type:email","search_mode":"semantic",
		"filters":[{"dimension":"message_type","values":["sms"]}]
	}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.Empty(body.Rows, "a disjoint operator/filter intersection matches nothing")
	assertions.False(body.CandidatePoolSaturated)
	assertions.Empty(body.NextCursor)
	requirements.NotNil(body.SearchProvenance.VectorGeneration)
	assertions.Equal(int64(7), *body.SearchProvenance.VectorGeneration)
	assertions.NotEmpty(body.CandidateSnapshotID, "empty candidate sets still issue a snapshot for follow-up calls")
	assertions.Zero(backend.searchLimit, "the vector index must not run a broadened query")
}

func TestExploreHybridDisjointFilterShortCircuitsToEmptyCandidates(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := &fakeFusingBackend{
		fakeVectorBackend: &fakeVectorBackend{
			active: &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		},
		fusedHits: []vector.FusedHit{{MessageID: 3, RRFScore: .9, VectorScore: .9}},
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	store := &mockStore{messages: []APIMessage{{ID: 3}}, total: 1, stats: &StoreStats{}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store,
		Engine: newExploreDuckDBFixture(t), HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/explore", `{
		"query":"alpha message_type:email","search_mode":"hybrid",
		"filters":[{"dimension":"message_type","values":["sms"]}]
	}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.Empty(body.Rows)
	requirements.NotNil(body.SearchProvenance.VectorGeneration)
	assertions.Equal(int64(7), *body.SearchProvenance.VectorGeneration)
	assertions.NotEmpty(body.SearchProvenance.LexicalIndexRevision, "hybrid provenance keeps the lexical revision")
	assertions.Zero(backend.fusedCalls, "the fused query must not run for an unsatisfiable predicate")
}

func TestApplySemanticDeletionScope(t *testing.T) {
	tests := []struct {
		name         string
		searchMode   string
		deletion     query.DeletionFilter
		wantScope    string
		wantDeletion query.DeletionFilter
	}{
		{name: "full text leaves the context unrestricted", searchMode: exploreSearchModeFullText, deletion: query.DeletionAny, wantScope: "", wantDeletion: query.DeletionAny},
		{name: "no search leaves the context unrestricted", searchMode: "", deletion: query.DeletionAny, wantScope: "", wantDeletion: query.DeletionAny},
		{name: "semantic narrows unrestricted to active", searchMode: exploreSearchModeSemantic, deletion: query.DeletionAny, wantScope: "active", wantDeletion: query.DeletionActive},
		{name: "hybrid narrows unrestricted to active", searchMode: exploreSearchModeHybrid, deletion: query.DeletionAny, wantScope: "active", wantDeletion: query.DeletionActive},
		{name: "semantic keeps explicit active", searchMode: exploreSearchModeSemantic, deletion: query.DeletionActive, wantScope: "active", wantDeletion: query.DeletionActive},
		{name: "semantic leaves deleted for the resolver to reject", searchMode: exploreSearchModeSemantic, deletion: query.DeletionDeleted, wantScope: "", wantDeletion: query.DeletionDeleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			context := query.Context{Deletion: tt.deletion}
			scope := applySemanticDeletionScope(tt.searchMode, &context)
			assert.Equal(t, tt.wantScope, scope)
			assert.Equal(t, tt.wantDeletion, context.Deletion)
		})
	}
}

func TestExploreSemanticRejectsDeletedOnlyFilter(t *testing.T) {
	srv := newReviewSemanticServer(t)
	response := postExploreJSON(t, srv, "/api/v1/explore", `{
		"query":"alpha","search_mode":"semantic",
		"filters":[{"dimension":"deletion","values":["deleted"]}]
	}`)
	assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "semantic_deletion_unsupported")
	assert.Contains(t, response.Body.String(), "active messages only")
}

func TestExploreSemanticNarrowsUnrestrictedDeletionToActive(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	base := newExploreDuckDBFixture(t)
	engine := &recordingExploreEngine{Engine: base, Explorer: base}
	backend := &fakeVectorBackend{
		active:     &vector.Generation{ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive},
		searchHits: []vector.Hit{{MessageID: 1, Score: .9, Rank: 1}},
	}
	hybridEngine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"})
	store := &mockStore{messages: []APIMessage{{ID: 1}}, total: 1, stats: &StoreStats{}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: store,
		Engine: engine, HybridEngine: hybridEngine, Backend: backend, Logger: testLogger(),
	})

	semantic := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"semantic"}`)
	requirements.Equal(http.StatusOK, semantic.Code, semantic.Body.String())
	var semanticBody ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(semantic.Body.Bytes(), &semanticBody))
	assertions.Equal("active", semanticBody.SearchDeletionScope, "the narrowing must be declared, not silent")
	assertions.Equal(query.DeletionActive, engine.request.Context.Deletion, "the analytical context must match the declared scope")

	fullText := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"alpha","search_mode":"full_text"}`)
	requirements.Equal(http.StatusOK, fullText.Code, fullText.Body.String())
	var raw map[string]json.RawMessage
	requirements.NoError(json.Unmarshal(fullText.Body.Bytes(), &raw))
	assertions.NotContains(raw, "search_deletion_scope", "full text keeps the unrestricted deletion context")
	assertions.Equal(query.DeletionAny, engine.request.Context.Deletion)

	groups := postExploreJSON(t, srv, "/api/v1/explore/groups", `{"grouping":["source"],"query":"alpha","search_mode":"semantic"}`)
	requirements.Equal(http.StatusOK, groups.Code, groups.Body.String())
	var groupsBody ExploreGroupsHTTPResponse
	requirements.NoError(json.Unmarshal(groups.Body.Bytes(), &groupsBody))
	assertions.Equal("active", groupsBody.SearchDeletionScope)
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
