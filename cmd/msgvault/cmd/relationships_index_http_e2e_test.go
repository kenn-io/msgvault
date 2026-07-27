package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type relationshipIndexHTTPFixture struct {
	store          *store.Store
	dbPath         string
	analyticsDir   string
	sourceID       int64
	ownerID        int64
	personID       int64
	lateID         int64
	conversationID int64
	outgoingID     int64
	firstAt        time.Time
	lastAt         time.Time
}

func newRelationshipIndexHTTPFixture(t *testing.T) relationshipIndexHTTPFixture {
	t.Helper()

	root := t.TempDir()
	dbPath := filepath.Join(root, "msgvault.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	require.NoError(t, st.InitSchema())

	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(t, err)
	ownerID, err := st.EnsureParticipant("owner@example.test", "Owner", "example.test")
	require.NoError(t, err)
	personID, err := st.EnsureParticipant("alex@people.test", "Alex Example", "people.test")
	require.NoError(t, err)
	lateID, err := st.EnsureParticipant("late-member@late.test", "Late Member", "late.test")
	require.NoError(t, err)
	require.NoError(t, st.AddAccountIdentity(source.ID, "owner@example.test", "test"))

	conversationID, err := st.EnsureConversation(source.ID, "thread-index-http", "Index HTTP")
	require.NoError(t, err)
	require.NoError(t, st.EnsureConversationParticipant(conversationID, ownerID, "member"))
	require.NoError(t, st.EnsureConversationParticipant(conversationID, personID, "member"))

	firstAt := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	lastAt := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	outgoingID := insertRelationshipIndexMessage(t, st, store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "outgoing",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: firstAt, Valid: true},
		SenderID:        sql.NullInt64{Int64: ownerID, Valid: true},
		IsFromMe:        true,
		Subject:         sql.NullString{String: "Needle project", Valid: true},
		Snippet:         sql.NullString{String: "needle candidate", Valid: true},
		SizeEstimate:    100,
	})
	require.NoError(t, st.ReplaceMessageRecipients(outgoingID, "to", []int64{personID}, []string{"Alex Example"}))

	incomingID := insertRelationshipIndexMessage(t, st, store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "incoming",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: lastAt, Valid: true},
		SenderID:        sql.NullInt64{Int64: personID, Valid: true},
		IsFromMe:        false,
		Subject:         sql.NullString{String: "Reply", Valid: true},
		Snippet:         sql.NullString{String: "ordinary reply", Valid: true},
		SizeEstimate:    200,
	})
	require.NoError(t, st.ReplaceMessageRecipients(incomingID, "to", []int64{ownerID}, []string{"Owner"}))

	require.NoError(t, st.UpsertMessageBody(
		outgoingID,
		sql.NullString{String: "needle project body", Valid: true},
		sql.NullString{},
	))
	_, err = st.BackfillFTS(nil)
	require.NoError(t, err)

	analyticsDir := filepath.Join(root, "analytics")
	result, err := buildCache(dbPath, analyticsDir, true)
	require.NoError(t, err)
	require.False(t, result.Skipped)

	return relationshipIndexHTTPFixture{
		store: st, dbPath: dbPath, analyticsDir: analyticsDir,
		sourceID: source.ID, ownerID: ownerID, personID: personID, lateID: lateID, conversationID: conversationID,
		outgoingID: outgoingID, firstAt: firstAt, lastAt: lastAt,
	}
}

func insertRelationshipIndexMessage(t *testing.T, st *store.Store, message store.Message) int64 {
	t.Helper()
	id, err := st.UpsertMessage(&message)
	require.NoError(t, err)
	return id
}

func (f relationshipIndexHTTPFixture) newServer(t *testing.T, disableLegacyViews bool) (*api.Server, *query.DuckDBEngine) {
	t.Helper()
	engine, err := query.NewDuckDBEngine(
		f.analyticsDir,
		f.dbPath,
		f.store.DB(),
		query.DuckDBOptions{
			DisableSQLiteScanner:         true,
			DisableLegacyAnalyticalViews: disableLegacyViews,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	cfg := &config.Config{
		HomeDir: f.analyticsDir,
		Data:    config.DataConfig{DataDir: filepath.Dir(f.dbPath)},
		Server:  config.ServerConfig{APIPort: 8080},
	}
	server := api.NewServerWithOptions(api.ServerOptions{
		Config: cfg,
		Store: &storeAPIAdapter{
			store:        f.store,
			analyticsDir: f.analyticsDir,
		},
		Engine: engine,
		Logger: slog.New(slog.DiscardHandler),
	})
	return server, engine
}

func relationshipIndexHTTPJSON(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var content io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		content = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, content)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeRelationshipIndexHTTP[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result T
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	return result
}

func TestRelationshipIndexMigratedHTTPRoutesNeedNoLegacyViews(t *testing.T) {
	fixture := newRelationshipIndexHTTPFixture(t)
	server, _ := fixture.newServer(t, true)
	handler := server.Router()
	personPath := "/api/v1/people/" + strconv.FormatInt(fixture.personID, 10)

	relationships := decodeRelationshipIndexHTTP[api.RelationshipsHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/relationships", map[string]any{
			"show_all": true,
			"limit":    25,
		}),
	)
	require.Len(t, relationships.Rows, 1)
	assert.Equal(t, int64(1), relationships.TotalCount)
	assert.NotEmpty(t, relationships.CacheRevision)
	assert.Equal(t, fixture.personID, relationships.Rows[0].CanonicalID)
	assert.Equal(t, []int64{fixture.personID}, relationships.Rows[0].MemberIDs)
	assert.Equal(t, fixture.lastAt, relationships.Rows[0].LastAt)
	assert.Equal(t, int64(1), relationships.Rows[0].Signals.SentCount)
	assert.Equal(t, int64(0), relationships.Rows[0].Signals.MeetingCount)

	people := decodeRelationshipIndexHTTP[api.PersonSearchHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/people/search", map[string]any{
			"predicate":      map[string]any{},
			"identity_query": "alex",
			"sort":           map[string]string{"field": "activity_count", "direction": "desc"},
			"limit":          25,
		}),
	)
	require.Len(t, people.Rows, 1)
	assert.Equal(t, int64(1), people.TotalCount)
	assert.NotEmpty(t, people.CacheRevision)
	assert.Equal(t, fixture.personID, people.Rows[0].ID)
	assert.Equal(t, int64(2), people.Rows[0].ActivityCount)
	assert.Equal(t, fixture.firstAt, people.Rows[0].FirstAt)
	assert.Equal(t, fixture.lastAt, people.Rows[0].LastAt)

	person := decodeRelationshipIndexHTTP[query.PersonSummary](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodGet, personPath, nil),
	)
	assert.Equal(t, fixture.personID, person.ID)
	assert.Equal(t, int64(2), person.ActivityCount)
	assert.NotEmpty(t, person.CacheRevision)

	domains := decodeRelationshipIndexHTTP[api.DomainSearchHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/domains/search", map[string]any{
			"predicate":      map[string]any{},
			"identity_query": "people.test",
			"sort":           map[string]string{"field": "activity_count", "direction": "desc"},
			"limit":          25,
		}),
	)
	require.Len(t, domains.Rows, 1)
	assert.Equal(t, int64(1), domains.TotalCount)
	assert.NotEmpty(t, domains.CacheRevision)
	assert.Equal(t, "people.test", domains.Rows[0].Domain)
	assert.Equal(t, int64(2), domains.Rows[0].ActivityCount)
	assert.Equal(t, int64(1), domains.Rows[0].PersonCount)

	domain := decodeRelationshipIndexHTTP[query.DomainSummary](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodGet, "/api/v1/domains/people.test", nil),
	)
	assert.Equal(t, "people.test", domain.Domain)
	assert.Equal(t, int64(2), domain.ActivityCount)
	assert.NotEmpty(t, domain.CacheRevision)

	filters := []map[string]any{
		{"dimension": "source", "values": []string{strconv.FormatInt(fixture.sourceID, 10)}},
		{"dimension": "participant", "values": []string{strconv.FormatInt(fixture.personID, 10)}},
		{"dimension": "domain", "values": []string{"people.test"}},
		{"dimension": "message_type", "values": []string{"email"}},
		{"dimension": "after", "values": []string{"2026-07-20T09:00:00Z"}},
		{"dimension": "before", "values": []string{"2026-07-20T11:00:00Z"}},
		{"dimension": "deletion", "values": []string{"active"}},
	}
	filteredPredicate := map[string]any{
		"filters":     filters,
		"query":       "needle",
		"search_mode": "full_text",
	}
	filteredPeople := decodeRelationshipIndexHTTP[api.PersonSearchHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/people/search", map[string]any{
			"predicate":      filteredPredicate,
			"identity_query": "alex",
			"sort":           map[string]string{"field": "activity_count", "direction": "desc"},
			"limit":          25,
		}),
	)
	require.Len(t, filteredPeople.Rows, 1)
	assert.Equal(t, int64(1), filteredPeople.TotalCount)
	assert.Equal(t, fixture.personID, filteredPeople.Rows[0].ID)
	assert.Equal(t, int64(1), filteredPeople.Rows[0].ActivityCount)

	personSummary := decodeRelationshipIndexHTTP[api.PersonContextSummaryHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, personPath+"/summary", filteredPredicate),
	)
	assert.Equal(t, fixture.personID, personSummary.Summary.ID)
	assert.Equal(t, int64(1), personSummary.Summary.ActivityCount)
	assert.NotEmpty(t, personSummary.CacheRevision)

	filteredDomains := decodeRelationshipIndexHTTP[api.DomainSearchHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/domains/search", map[string]any{
			"predicate":      filteredPredicate,
			"identity_query": "people.test",
			"sort":           map[string]string{"field": "activity_count", "direction": "desc"},
			"limit":          25,
		}),
	)
	require.Len(t, filteredDomains.Rows, 1)
	assert.Equal(t, int64(1), filteredDomains.TotalCount)
	assert.Equal(t, "people.test", filteredDomains.Rows[0].Domain)
	assert.Equal(t, int64(1), filteredDomains.Rows[0].ActivityCount)

	domainSummary := decodeRelationshipIndexHTTP[api.DomainContextSummaryHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/domains/people.test/summary", filteredPredicate),
	)
	assert.Equal(t, "people.test", domainSummary.Summary.Domain)
	assert.Equal(t, int64(1), domainSummary.Summary.ActivityCount)
	assert.NotEmpty(t, domainSummary.CacheRevision)

	filteredRelationships := decodeRelationshipIndexHTTP[api.RelationshipsHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/relationships", map[string]any{
			"filters":  filters,
			"show_all": true,
			"limit":    25,
		}),
	)
	require.Len(t, filteredRelationships.Rows, 1)
	assert.Equal(t, int64(1), filteredRelationships.TotalCount)
	assert.Equal(t, fixture.personID, filteredRelationships.Rows[0].CanonicalID)
	assert.Equal(t, int64(1), filteredRelationships.Rows[0].Signals.SentCount)
	assert.NotEmpty(t, filteredRelationships.CacheRevision)
}

func TestRelationshipIndexLeavesLegacyRoutesOutsideMigrationBoundary(t *testing.T) {
	fixture := newRelationshipIndexHTTPFixture(t)
	normalServer, _ := fixture.newServer(t, false)
	guardServer, _ := fixture.newServer(t, true)

	personID := strconv.FormatInt(fixture.personID, 10)
	routes := []struct {
		name string
		path string
		body map[string]any
	}{
		{name: "person timeline", path: "/api/v1/people/" + personID + "/timeline", body: map[string]any{}},
		{name: "domain timeline", path: "/api/v1/domains/people.test/timeline", body: map[string]any{}},
		{name: "relationship timeline", path: "/api/v1/relationships/" + personID + "/timeline", body: map[string]any{}},
		{name: "person files", path: "/api/v1/people/" + personID + "/files/search", body: map[string]any{"predicate": map[string]any{}}},
		{name: "domain files", path: "/api/v1/domains/people.test/files/search", body: map[string]any{"predicate": map[string]any{}}},
		{name: "global files", path: "/api/v1/files/search", body: map[string]any{"predicate": map[string]any{}}},
	}

	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			normal := relationshipIndexHTTPJSON(t, normalServer.Router(), http.MethodPost, route.path, route.body)
			require.Equal(t, http.StatusOK, normal.Code, normal.Body.String())

			guarded := relationshipIndexHTTPJSON(t, guardServer.Router(), http.MethodPost, route.path, route.body)
			assert.Equal(t, http.StatusInternalServerError, guarded.Code, guarded.Body.String())
			var response struct {
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(guarded.Body.Bytes(), &response))
			assert.Equal(t, "explore_failed", response.Error)
		})
	}
}

func TestRelationshipIndexDerivedRefreshHealsLateConversationMembership(t *testing.T) {
	fixture := newRelationshipIndexHTTPFixture(t)
	before, err := query.ReadCacheSyncState(fixture.analyticsDir)
	require.NoError(t, err)
	beforeRevision := before.Revision()

	require.NoError(t, fixture.store.EnsureConversationParticipant(
		fixture.conversationID,
		fixture.lateID,
		"member",
	))

	staleness := cacheNeedsBuild(fixture.dbPath, fixture.analyticsDir)
	require.True(t, staleness.NeedsBuild)
	assert.True(t, staleness.HasConversationParticipantDrift)
	assert.False(t, staleness.FullRebuild)

	result, err := buildCacheDerivedOnly(fixture.dbPath, fixture.analyticsDir)
	require.NoError(t, err)
	assert.True(t, result.IdentityOnly)

	after, err := query.ReadCacheSyncState(fixture.analyticsDir)
	require.NoError(t, err)
	assert.Equal(t, before.LastMessageID, after.LastMessageID)
	assert.Equal(t, before.Stats, after.Stats)
	assert.NotEqual(t,
		before.ConversationParticipantsFingerprint,
		after.ConversationParticipantsFingerprint,
	)
	assert.False(t, after.PublishedAt.Before(before.PublishedAt))
	assert.Equal(t, time.Now().UTC().Format(time.DateOnly), after.RelationshipAnchorDate)
	assert.NotEqual(t, beforeRevision, after.Revision())

	inspectionDB, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspectionDB.Close()) })
	var lateEdgeCount int64
	require.NoError(t, inspectionDB.QueryRow(`
		SELECT count(*)
		FROM read_parquet(?)
		WHERE conversation_id = ? AND participant_id = ?
	`, filepath.Join(
		fixture.analyticsDir,
		identityindex.DatasetConversationEdges,
		"*.parquet",
	), fixture.conversationID, fixture.lateID).Scan(&lateEdgeCount))
	assert.Equal(t, int64(1), lateEdgeCount)

	_, engine := fixture.newServer(t, true)
	people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{
		Explore: query.ExploreRequest{Context: query.Context{
			ParticipantIDs: []int64{fixture.lateID},
		}},
		Sort: query.SortSpec{Field: "activity_count", Direction: "desc"},
		Page: query.PageSpec{Limit: 25},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), people.TotalCount)
	assert.ElementsMatch(t, []int64{fixture.ownerID, fixture.personID}, personSummaryIDs(people.Rows))
	for _, person := range people.Rows {
		assert.Equal(t, int64(2), person.ActivityCount)
		assert.NotEqual(t, fixture.lateID, person.ID,
			"conversation-only membership must not enter non-chat people fan-out")
	}

	domains, err := engine.SearchDomains(t.Context(), query.DomainSearchRequest{
		Explore: query.ExploreRequest{Context: query.Context{
			ParticipantIDs: []int64{fixture.lateID},
		}},
		Sort: query.SortSpec{Field: "activity_count", Direction: "desc"},
		Page: query.PageSpec{Limit: 25},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(3), domains.TotalCount)
	assert.ElementsMatch(t,
		[]string{"example.test", "people.test", "late.test"},
		domainSummaryNames(domains.Rows),
	)
	for _, domain := range domains.Rows {
		assert.Equal(t, int64(2), domain.ActivityCount)
	}
}

func personSummaryIDs(rows []query.PersonSummary) []int64 {
	ids := make([]int64, len(rows))
	for index := range rows {
		ids[index] = rows[index].ID
	}
	return ids
}

func domainSummaryNames(rows []query.DomainSummary) []string {
	domains := make([]string, len(rows))
	for index := range rows {
		domains[index] = rows[index].Domain
	}
	return domains
}
