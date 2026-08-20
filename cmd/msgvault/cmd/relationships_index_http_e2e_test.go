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
	requirementsForTest := require.New(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, "msgvault.db")
	st, err := store.OpenForTest(dbPath)
	requirementsForTest.NoError(err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	requirementsForTest.NoError(st.InitSchema())

	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	requirementsForTest.NoError(err)
	ownerID, err := st.EnsureParticipant("owner@example.test", "Owner", "example.test")
	requirementsForTest.NoError(err)
	personID, err := st.EnsureParticipant("alex@people.test", "Alex Example", "people.test")
	requirementsForTest.NoError(err)
	lateID, err := st.EnsureParticipant("late-member@late.test", "Late Member", "late.test")
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(st.AddAccountIdentity(source.ID, "owner@example.test", "test"))

	conversationID, err := st.EnsureConversation(source.ID, "thread-index-http", "Index HTTP")
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(st.EnsureConversationParticipant(conversationID, ownerID, "member"))
	requirementsForTest.NoError(st.EnsureConversationParticipant(conversationID, personID, "member"))

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
	requirementsForTest.NoError(st.ReplaceMessageRecipients(outgoingID, "to", []int64{personID}, []string{"Alex Example"}))

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
	requirementsForTest.NoError(st.ReplaceMessageRecipients(incomingID, "to", []int64{ownerID}, []string{"Owner"}))
	requirementsForTest.NoError(st.UpsertMessageBody(
		outgoingID,
		sql.NullString{String: "needle project body", Valid: true},
		sql.NullString{},
	))
	_, err = st.BackfillFTS(nil)
	requirementsForTest.NoError(err)

	analyticsDir := filepath.Join(root, "analytics")
	result, err := buildCache(dbPath, analyticsDir, true)
	requirementsForTest.NoError(err)
	requirementsForTest.False(result.Skipped)

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

func (f relationshipIndexHTTPFixture) newServer(t *testing.T, disableLegacyViews bool) *api.Server {
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
	return server
}

func relationshipIndexFilter(dimension string, values ...string) map[string]any {
	return map[string]any{"dimension": dimension, "values": values}
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

func TestRelationshipIndexUnfilteredRoutesNeedNoAnalyticalViews(t *testing.T) {
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)
	fixture := newRelationshipIndexHTTPFixture(t)
	server := fixture.newServer(t, true)
	handler := server.Router()

	relationships := decodeRelationshipIndexHTTP[api.RelationshipsHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/relationships", map[string]any{
			"show_all": true,
			"limit":    25,
		}),
	)
	requirementsForTest.Len(relationships.Rows, 1)
	assertionsForTest.Equal(int64(1), relationships.TotalCount)
	assertionsForTest.NotEmpty(relationships.CacheRevision)
	assertionsForTest.Equal(fixture.personID, relationships.Rows[0].CanonicalID)
	assertionsForTest.Equal([]int64{fixture.personID}, relationships.Rows[0].MemberIDs)
	assertionsForTest.Equal(fixture.lastAt, relationships.Rows[0].LastAt)
	assertionsForTest.Equal(int64(1), relationships.Rows[0].Signals.SentCount)
	assertionsForTest.Equal(int64(0), relationships.Rows[0].Signals.MeetingCount)

	people := decodeRelationshipIndexHTTP[api.ParticipantSearchHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/participants/search", map[string]any{
			"predicate":      map[string]any{},
			"identity_query": "alex",
			"sort":           map[string]string{"field": "activity_count", "direction": "desc"},
			"limit":          25,
		}),
	)
	requirementsForTest.Len(people.Rows, 1)
	assertionsForTest.Equal(int64(1), people.TotalCount)
	assertionsForTest.NotEmpty(people.CacheRevision)
	assertionsForTest.Equal(fixture.personID, people.Rows[0].ID)
	assertionsForTest.Equal(int64(2), people.Rows[0].ActivityCount)
	assertionsForTest.Equal(fixture.firstAt, people.Rows[0].FirstAt)
	assertionsForTest.Equal(fixture.lastAt, people.Rows[0].LastAt)

	domains := decodeRelationshipIndexHTTP[api.DomainSearchHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/domains/search", map[string]any{
			"predicate":      map[string]any{},
			"identity_query": "people.test",
			"sort":           map[string]string{"field": "activity_count", "direction": "desc"},
			"limit":          25,
		}),
	)
	requirementsForTest.Len(domains.Rows, 1)
	assertionsForTest.Equal(int64(1), domains.TotalCount)
	assertionsForTest.NotEmpty(domains.CacheRevision)
	assertionsForTest.Equal("people.test", domains.Rows[0].Domain)
	assertionsForTest.Equal(int64(2), domains.Rows[0].ActivityCount)
	assertionsForTest.Equal(int64(1), domains.Rows[0].PersonCount)

	personDetail := decodeRelationshipIndexHTTP[query.PersonSummary](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodGet,
			"/api/v1/participants/"+strconv.FormatInt(fixture.personID, 10), nil),
	)
	assertionsForTest.Equal(fixture.personID, personDetail.ID)
	assertionsForTest.Equal(int64(2), personDetail.ActivityCount)
	assertionsForTest.Equal("Alex Example", personDetail.DisplayLabel)

	timeline := decodeRelationshipIndexHTTP[api.RelationshipTimelineHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost,
			"/api/v1/relationships/"+strconv.FormatInt(fixture.personID, 10)+"/timeline",
			map[string]any{"limit": 10}),
	)
	requirementsForTest.Len(timeline.Rows, 2)
	assertionsForTest.Equal(int64(2), timeline.TotalCount)
	assertionsForTest.Equal("message:"+strconv.FormatInt(fixture.outgoingID, 10), timeline.Rows[1].Key)
	assertionsForTest.Equal("Needle project", timeline.Rows[1].Title)
}

// TestRelationshipIndexFilteredRoutesServeThroughExploreEngine exercises the
// same list routes under a filtered predicate, which routes through the
// explore logical-entry machinery on a standard engine.
// TestRelationshipIndexLeavesLegacyRoutesOutsideMigrationBoundary pins that
// these filtered requests fail closed when the analytical views are absent.
func TestRelationshipIndexFilteredRoutesServeThroughExploreEngine(t *testing.T) {
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)
	fixture := newRelationshipIndexHTTPFixture(t)
	server := fixture.newServer(t, false)
	handler := server.Router()

	filters := []map[string]any{
		relationshipIndexFilter("source", strconv.FormatInt(fixture.sourceID, 10)),
		relationshipIndexFilter("participant", strconv.FormatInt(fixture.personID, 10)),
		relationshipIndexFilter("domain", "people.test"),
		relationshipIndexFilter("message_type", "email"),
		relationshipIndexFilter("after", "2026-07-20T09:00:00Z"),
		relationshipIndexFilter("before", "2026-07-20T11:00:00Z"),
		relationshipIndexFilter("deletion", "active"),
	}
	filteredPredicate := map[string]any{
		"filters":     filters,
		"query":       "needle",
		"search_mode": "full_text",
	}
	filteredPeople := decodeRelationshipIndexHTTP[api.ParticipantSearchHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/participants/search", map[string]any{
			"predicate":      filteredPredicate,
			"identity_query": "alex",
			"sort":           map[string]string{"field": "activity_count", "direction": "desc"},
			"limit":          25,
		}),
	)
	requirementsForTest.Len(filteredPeople.Rows, 1)
	assertionsForTest.Equal(int64(1), filteredPeople.TotalCount)
	assertionsForTest.Equal(fixture.personID, filteredPeople.Rows[0].ID)
	assertionsForTest.Equal(int64(1), filteredPeople.Rows[0].ActivityCount)

	filteredDomains := decodeRelationshipIndexHTTP[api.DomainSearchHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/domains/search", map[string]any{
			"predicate":      filteredPredicate,
			"identity_query": "people.test",
			"sort":           map[string]string{"field": "activity_count", "direction": "desc"},
			"limit":          25,
		}),
	)
	requirementsForTest.Len(filteredDomains.Rows, 1)
	assertionsForTest.Equal(int64(1), filteredDomains.TotalCount)
	assertionsForTest.Equal("people.test", filteredDomains.Rows[0].Domain)
	assertionsForTest.Equal(int64(1), filteredDomains.Rows[0].ActivityCount)

	filteredRelationships := decodeRelationshipIndexHTTP[api.RelationshipsHTTPResponse](
		t,
		relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/relationships", map[string]any{
			"filters":  filters,
			"show_all": true,
			"limit":    25,
		}),
	)
	requirementsForTest.Len(filteredRelationships.Rows, 1)
	assertionsForTest.Equal(int64(1), filteredRelationships.TotalCount)
	assertionsForTest.Equal(fixture.personID, filteredRelationships.Rows[0].CanonicalID)
	assertionsForTest.Equal(int64(1), filteredRelationships.Rows[0].Signals.SentCount)
	assertionsForTest.NotEmpty(filteredRelationships.CacheRevision)
}

func TestRelationshipIndexLeavesLegacyRoutesOutsideMigrationBoundary(t *testing.T) {
	fixture := newRelationshipIndexHTTPFixture(t)
	normalServer := fixture.newServer(t, false)
	guardServer := fixture.newServer(t, true)

	personID := strconv.FormatInt(fixture.personID, 10)
	filteredPredicate := map[string]any{
		"filters": []map[string]any{
			relationshipIndexFilter("message_type", "email"),
		},
	}
	routes := []struct {
		name   string
		method string
		path   string
		body   map[string]any
	}{
		{name: "filtered people search", method: http.MethodPost, path: "/api/v1/participants/search", body: map[string]any{"predicate": filteredPredicate, "identity_query": "alex"}},
		{name: "filtered domain search", method: http.MethodPost, path: "/api/v1/domains/search", body: map[string]any{"predicate": filteredPredicate, "identity_query": "people.test"}},
		{name: "filtered relationships", method: http.MethodPost, path: "/api/v1/relationships", body: map[string]any{"filters": filteredPredicate["filters"], "show_all": true}},
		{name: "person summary", method: http.MethodPost, path: "/api/v1/participants/" + personID + "/summary", body: map[string]any{}},
		{name: "domain detail", method: http.MethodGet, path: "/api/v1/domains/people.test"},
		{name: "domain summary", method: http.MethodPost, path: "/api/v1/domains/people.test/summary", body: map[string]any{}},
		{name: "person timeline", method: http.MethodPost, path: "/api/v1/participants/" + personID + "/timeline", body: map[string]any{}},
		{name: "domain timeline", method: http.MethodPost, path: "/api/v1/domains/people.test/timeline", body: map[string]any{}},
		{name: "person files", method: http.MethodPost, path: "/api/v1/participants/" + personID + "/files/search", body: map[string]any{"predicate": map[string]any{}}},
		{name: "domain files", method: http.MethodPost, path: "/api/v1/domains/people.test/files/search", body: map[string]any{"predicate": map[string]any{}}},
		{name: "global files", method: http.MethodPost, path: "/api/v1/files/search", body: map[string]any{"predicate": map[string]any{}}},
	}

	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			normal := relationshipIndexHTTPJSON(t, normalServer.Router(), route.method, route.path, route.body)
			requirements.Equal(http.StatusOK, normal.Code, normal.Body.String())

			guarded := relationshipIndexHTTPJSON(t, guardServer.Router(), route.method, route.path, route.body)
			assertions.Equal(http.StatusInternalServerError, guarded.Code, guarded.Body.String())
			var response struct {
				Error string `json:"error"`
			}
			requirements.NoError(json.Unmarshal(guarded.Body.Bytes(), &response))
			assertions.Equal("explore_failed", response.Error)
		})
	}
}
