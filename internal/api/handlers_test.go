package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// stubEmbedder is an EmbeddingClient placeholder for tests where the
// engine never reaches the embed step (e.g. ResolveActiveForFingerprint
// fails first). Calling Embed signals a test bug — guard with a t.Fatal-
// style failure rather than returning silently.
type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return nil, errors.New("stubEmbedder.Embed should not be called in this test")
}

func newTestServerWithMockStore(t *testing.T) (*Server, *mockStore) {
	t.Helper()

	store := &mockStore{
		stats: &StoreStats{
			MessageCount:       10,
			SourceDeletedCount: 4,
			ThreadCount:        5,
			SourceCount:        1,
			LabelCount:         3,
			AttachmentCount:    2,
			DatabaseSize:       1024,
		},
		messages: []APIMessage{
			{
				ID:             1,
				Subject:        "Test Subject",
				From:           "sender@example.com",
				To:             []string{"recipient@example.com"},
				SentAt:         time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				Snippet:        "This is a test message snippet",
				Labels:         []string{"INBOX"},
				HasAttachments: false,
				SizeEstimate:   1024,
				Body:           "This is the full message body text.",
				Attachments:    nil,
			},
		},
		total: 1,
	}

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Accounts: []config.AccountSchedule{
			{Email: "test@gmail.com", Schedule: "0 2 * * *", Enabled: true},
		},
	}

	sched := newMockScheduler()
	sched.scheduled["test@gmail.com"] = true

	srv := NewServer(cfg, store, sched, testLogger())
	return srv, store
}

func newCLIHandlerTestServer(st *mockStore) *Server {
	return NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())
}

func servePOSTTestRequest(srv *Server, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	return resp
}

func requireNDJSONResponse(t *testing.T, resp *httptest.ResponseRecorder) {
	t.Helper()

	require.Equal(t, http.StatusOK, resp.Code, "status: %s", resp.Body.String())
	assert.Contains(t, resp.Header().Get("Content-Type"), "application/x-ndjson", "content type")
}

func decodeNDJSONEvents[T any](t *testing.T, body io.Reader) []T {
	t.Helper()

	var events []T
	dec := json.NewDecoder(body)
	for {
		var event T
		err := dec.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err, "decode event")
		events = append(events, event)
	}
	return events
}

func TestHandleStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	bodyBytes := w.Body.Bytes()

	var resp StatsResponse
	require.NoError(json.Unmarshal(bodyBytes, &resp), "failed to decode response")

	assert.Equal(int64(10), resp.TotalMessages, "total_messages")
	assert.Equal(int64(10), resp.ActiveMessages, "active_messages")
	assert.Equal(int64(4), resp.SourceDeletedMessages, "source_deleted_messages")
	assert.Equal(int64(1), resp.TotalAccounts, "total_accounts")

	// No backend wired → vector_search field must be ABSENT, not
	// null. Re-decode into raw RawMessage so we distinguish the two:
	// `omitempty` plus a nil pointer drops the key entirely, while
	// any encoder bug that emits `"vector_search": null` would still
	// leave resp.VectorSearch == nil.
	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal(bodyBytes, &raw), "decode raw")
	_, exists := raw["vector_search"]
	assert.False(exists, "vector_search key present in JSON; want omitted entirely (raw=%s)", string(raw["vector_search"]))
}

func TestHandleCLIStatsCollectionScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	_, err = st.CreateCollection("Important", "important mail", []int64{src.ID})
	require.NoError(err, "CreateCollection")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/stats?collection=Important", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp cliStatsResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal("Important", resp.ScopeLabel, "scope label")
	assert.Equal(1, resp.ScopeSourceCount, "scope source count")
	assert.Equal(int64(1), resp.Stats.TotalAccounts, "scoped account count")
	assert.Equal(int64(0), resp.Stats.TotalMessages, "scoped message count")
}

func TestHandleCLIStatsAccountScopeIncludesAssociatedCalendarSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	gmail, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource gmail")
	calendar, err := st.GetOrCreateSource(gcal.SourceType, "alice@example.com/primary")
	require.NoError(err, "GetOrCreateSource calendar")
	require.NotEqual(gmail.ID, calendar.ID, "fixture source IDs")
	calendarConfig, err := json.Marshal(map[string]string{
		"account_email": "alice@example.com",
		"calendar_id":   "primary",
	})
	require.NoError(err, "marshal calendar config")
	require.NoError(st.UpdateSourceSyncConfig(calendar.ID, string(calendarConfig)), "UpdateSourceSyncConfig")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/stats?account=alice@example.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status: %s", w.Body.String())

	var resp cliStatsResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal("alice@example.com", resp.ScopeLabel, "scope label")
	assert.Equal(2, resp.ScopeSourceCount, "scope source count")
	assert.Equal(int64(2), resp.Stats.TotalAccounts, "scoped account count")
}

func TestHandleCLIStatsAccountLookupErrorReturnsInternal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newTestServerWithMockStore(t)
	st.sourcesByLookupErr = errors.New("source index unavailable")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/stats?account=alice@example.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusInternalServerError, w.Code, "status: %s", w.Body.String())

	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode error response")
	assert.Equal("internal_error", resp.Error, "error code")
	assert.Equal("Failed to resolve CLI scope", resp.Message, "error message")
}

func TestHandleCLICreateDeletionManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	manifest := deletion.NewManifest("tui selection", []string{"gid1", "gid2"})
	manifest.CreatedBy = "tui"
	var saved *deletion.Manifest
	st := &mockStore{
		saveManifestFunc: func(_ context.Context, got *deletion.Manifest) error {
			saved = got
			return nil
		},
	}
	srv := newCLIHandlerTestServer(st)
	body, err := json.Marshal(manifest)
	require.NoError(err, "marshal manifest")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/deletion-manifests", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusOK, resp.Code, "status: %s", resp.Body.String())
	require.NotNil(saved, "saved manifest")
	assert.Equal(manifest.ID, saved.ID)
	assert.Equal("tui selection", saved.Description)
	assert.Equal([]string{"gid1", "gid2"}, saved.GmailIDs)
	assert.Equal(deletion.StatusPending, saved.Status)

	var decoded CLIDeletionManifestResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &decoded), "decode response")
	assert.Equal(manifest.ID, decoded.ID)
	assert.Equal(2, decoded.MessageCount)
}

func TestHandleCLICreateDeletionManifestRejectsTraversalID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	manifest := deletion.NewManifest("tui selection", []string{"gid1"})
	manifest.ID = "../../tokens/escape"
	saveCalled := false
	st := &mockStore{
		saveManifestFunc: func(_ context.Context, _ *deletion.Manifest) error {
			saveCalled = true
			return nil
		},
	}
	srv := newCLIHandlerTestServer(st)
	body, err := json.Marshal(manifest)
	require.NoError(err, "marshal manifest")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/deletion-manifests", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusBadRequest, resp.Code, "status: %s", resp.Body.String())
	assert.False(saveCalled, "save must not run for an invalid manifest ID")

	var decoded ErrorResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &decoded), "decode error response")
	assert.Equal("invalid_manifest_id", decoded.Error, "error code")
}

func TestHandleCLIIdentities(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	alice, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource alice")
	oldMbox, err := st.GetOrCreateSource("mbox", "old-mbox")
	require.NoError(err, "GetOrCreateSource old mbox")
	require.NoError(st.AddAccountIdentity(alice.ID, "alice@example.com", "manual"), "AddAccountIdentity")
	_, err = st.CreateCollection("IdentityScope", "identity test", []int64{alice.ID, oldMbox.ID})
	require.NoError(err, "CreateCollection")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/identities?collection=IdentityScope", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status: %s", w.Body.String())

	var resp cliIdentitiesResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	require.Len(resp.Rows, 2, "rows")

	assert.Equal("alice@example.com", resp.Rows[0].Account, "identity account")
	assert.Equal(alice.ID, resp.Rows[0].SourceID, "identity source ID")
	assert.Equal("gmail", resp.Rows[0].SourceType, "identity source type")
	assert.Equal("alice@example.com", resp.Rows[0].Identifier, "identity identifier")
	assert.Equal([]string{"manual"}, resp.Rows[0].Signals, "identity signals")
	require.NotNil(resp.Rows[0].ConfirmedAt, "identity confirmed_at")
	assert.False(resp.Rows[0].None, "identity none")

	assert.Equal("old-mbox", resp.Rows[1].Account, "none account")
	assert.Equal(oldMbox.ID, resp.Rows[1].SourceID, "none source ID")
	assert.Equal("mbox", resp.Rows[1].SourceType, "none source type")
	assert.Empty(resp.Rows[1].Identifier, "none identifier")
	assert.Empty(resp.Rows[1].Signals, "none signals")
	assert.Nil(resp.Rows[1].ConfirmedAt, "none confirmed_at")
	assert.True(resp.Rows[1].None, "none flag")
}

func TestHandleCLIIdentitiesPrimaryOnlyAccount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	alice, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource alice")
	cal, err := st.GetOrCreateSource(gcal.SourceType, "alice@example.com/primary")
	require.NoError(err, "GetOrCreateSource calendar")
	calendarConfig, err := json.Marshal(map[string]string{
		"account_email": "alice@example.com",
		"calendar_id":   "primary",
	})
	require.NoError(err, "marshal calendar config")
	require.NoError(st.UpdateSourceSyncConfig(cal.ID, string(calendarConfig)), "UpdateSourceSyncConfig")
	require.NoError(st.AddAccountIdentity(alice.ID, "alice@example.com", "manual"), "AddAccountIdentity alice")
	require.NoError(st.AddAccountIdentity(cal.ID, "calendar-self@example.com", "manual"), "AddAccountIdentity calendar")

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/cli/identities?account=alice@example.com&primary_only=true",
		nil,
	)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status: %s", w.Body.String())
	var resp cliIdentitiesResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	require.Len(resp.Rows, 1, "rows")
	assert.Equal(alice.ID, resp.Rows[0].SourceID, "primary source ID")
	assert.Equal("alice@example.com", resp.Rows[0].Account, "primary account")
	assert.Equal("alice@example.com", resp.Rows[0].Identifier, "primary identifier")
}

func TestHandleCLIIdentityAddAndRemove(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	alice, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource alice")

	addReq := strings.NewReader(`{
		"account": "alice@example.com",
		"identifier": "extra@example.com",
		"signal": "manual"
	}`)
	addHTTPReq := httptest.NewRequest(http.MethodPost, "/api/v1/cli/identities", addReq)
	addResp := httptest.NewRecorder()

	srv.Router().ServeHTTP(addResp, addHTTPReq)

	assert.Equal(http.StatusOK, addResp.Code, "add status: %s", addResp.Body.String())
	var addOut cliIdentityAddResponse
	require.NoError(json.NewDecoder(addResp.Body).Decode(&addOut), "decode add response")
	assert.Equal("alice@example.com", addOut.Account, "add account")
	assert.Equal("extra@example.com", addOut.Identifier, "add identifier")
	assert.Equal("manual", addOut.Signal, "add signal")
	assert.Equal("added", addOut.Outcome, "add outcome")

	identities, err := st.ListAccountIdentities(alice.ID)
	require.NoError(err, "ListAccountIdentities after add")
	require.Len(identities, 1, "identities after add")
	assert.Equal("extra@example.com", identities[0].Address, "stored address")

	removeReq := strings.NewReader(`{
		"account": "alice@example.com",
		"identifier": "extra@example.com"
	}`)
	removeHTTPReq := httptest.NewRequest(http.MethodDelete, "/api/v1/cli/identities", removeReq)
	removeResp := httptest.NewRecorder()

	srv.Router().ServeHTTP(removeResp, removeHTTPReq)

	assert.Equal(http.StatusOK, removeResp.Code, "remove status: %s", removeResp.Body.String())
	var removeOut cliIdentityRemoveResponse
	require.NoError(json.NewDecoder(removeResp.Body).Decode(&removeOut), "decode remove response")
	assert.Equal("alice@example.com", removeOut.Account, "remove account")
	assert.Equal("extra@example.com", removeOut.Identifier, "remove identifier")
	assert.Equal(int64(1), removeOut.Removed, "removed")
	assert.True(removeOut.NoIdentity, "no identity warning flag")

	identities, err = st.ListAccountIdentities(alice.ID)
	require.NoError(err, "ListAccountIdentities after remove")
	assert.Empty(identities, "identities after remove")
}

func TestHandleCLICollectionMutations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	alice, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource alice")
	bob, err := st.GetOrCreateSource("imap", "bob@example.com")
	require.NoError(err, "GetOrCreateSource bob")

	createReq := strings.NewReader(`{
		"name": "Team",
		"accounts": ["alice@example.com"]
	}`)
	createHTTPReq := httptest.NewRequest(http.MethodPost, "/api/v1/cli/collections", createReq)
	createResp := httptest.NewRecorder()

	srv.Router().ServeHTTP(createResp, createHTTPReq)

	assert.Equal(http.StatusOK, createResp.Code, "create status: %s", createResp.Body.String())
	var createOut struct {
		Name        string `json:"name"`
		SourceCount int    `json:"source_count"`
	}
	require.NoError(json.NewDecoder(createResp.Body).Decode(&createOut), "decode create response")
	assert.Equal("Team", createOut.Name, "create name")
	assert.Equal(1, createOut.SourceCount, "create source count")

	coll, err := st.GetCollectionByName("Team")
	require.NoError(err, "GetCollectionByName after create")
	assert.Equal([]int64{alice.ID}, coll.SourceIDs, "created sources")

	addReq := strings.NewReader(`{"accounts":["bob@example.com"]}`)
	addHTTPReq := httptest.NewRequest(http.MethodPatch, "/api/v1/cli/collections/Team/sources", addReq)
	addResp := httptest.NewRecorder()

	srv.Router().ServeHTTP(addResp, addHTTPReq)

	assert.Equal(http.StatusOK, addResp.Code, "add status: %s", addResp.Body.String())
	var addOut struct {
		Name        string `json:"name"`
		SourceCount int    `json:"source_count"`
	}
	require.NoError(json.NewDecoder(addResp.Body).Decode(&addOut), "decode add response")
	assert.Equal("Team", addOut.Name, "add name")
	assert.Equal(1, addOut.SourceCount, "add source count")

	coll, err = st.GetCollectionByName("Team")
	require.NoError(err, "GetCollectionByName after add")
	assert.Equal([]int64{alice.ID, bob.ID}, coll.SourceIDs, "sources after add")

	removeReq := strings.NewReader(`{"accounts":["alice@example.com"]}`)
	removeHTTPReq := httptest.NewRequest(http.MethodDelete, "/api/v1/cli/collections/Team/sources", removeReq)
	removeResp := httptest.NewRecorder()

	srv.Router().ServeHTTP(removeResp, removeHTTPReq)

	assert.Equal(http.StatusOK, removeResp.Code, "remove status: %s", removeResp.Body.String())
	var removeOut struct {
		Name        string `json:"name"`
		SourceCount int    `json:"source_count"`
	}
	require.NoError(json.NewDecoder(removeResp.Body).Decode(&removeOut), "decode remove response")
	assert.Equal("Team", removeOut.Name, "remove name")
	assert.Equal(1, removeOut.SourceCount, "remove source count")

	coll, err = st.GetCollectionByName("Team")
	require.NoError(err, "GetCollectionByName after remove")
	assert.Equal([]int64{bob.ID}, coll.SourceIDs, "sources after remove")

	deleteHTTPReq := httptest.NewRequest(http.MethodDelete, "/api/v1/cli/collections/Team", nil)
	deleteResp := httptest.NewRecorder()

	srv.Router().ServeHTTP(deleteResp, deleteHTTPReq)

	assert.Equal(http.StatusOK, deleteResp.Code, "delete status: %s", deleteResp.Body.String())
	var deleteOut struct {
		Name string `json:"name"`
	}
	require.NoError(json.NewDecoder(deleteResp.Body).Decode(&deleteOut), "decode delete response")
	assert.Equal("Team", deleteOut.Name, "delete name")

	_, err = st.GetCollectionByName("Team")
	require.ErrorIs(err, store.ErrCollectionNotFound, "collection deleted")
}

func TestOpenAPIExportsCLIIdentityContracts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assert.Equal(http.StatusOK, resp.Code, "openapi status: %s", resp.Body.String())
	var doc map[string]any
	require.NoError(json.NewDecoder(resp.Body).Decode(&doc), "decode openapi")
	assert.Equal("3.1.0", doc["openapi"], "openapi version")

	paths, ok := doc["paths"].(map[string]any)
	require.True(ok, "paths object")
	identityPath, ok := paths["/api/v1/cli/identities"].(map[string]any)
	require.True(ok, "identity path present")

	for _, method := range []string{"get", "post", "delete"} {
		op, ok := identityPath[method].(map[string]any)
		require.True(ok, "identity %s operation present", method)
		assert.Contains(op, "operationId", "identity %s operation id", method)
		assert.Contains(op, "responses", "identity %s responses", method)
	}
}

func TestOpenAPIExportsServerRouteTable(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusOK, resp.Code, "openapi status: %s", resp.Body.String())
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&doc), "decode openapi")

	expected := map[string][]string{
		"/health":                                {"get", "head"},
		daemon.DefaultPingPath:                   {"get"},
		"/api/v1/health":                         {"get"},
		"/api/v1/stats":                          {"get"},
		"/api/v1/cli/init-db":                    {"post"},
		"/api/v1/cli/stats":                      {"get"},
		"/api/v1/cli/search":                     {"get"},
		"/api/v1/cli/accounts":                   {"get"},
		"/api/v1/cli/build-cache":                {"post"},
		"/api/v1/cli/cache-stats":                {"get"},
		"/api/v1/cli/sync":                       {"post"},
		"/api/v1/cli/sync-full":                  {"post"},
		"/api/v1/cli/verify":                     {"post"},
		"/api/v1/cli/repair-encoding":            {"post"},
		"/api/v1/cli/message":                    {"get"},
		"/api/v1/cli/message/raw":                {"get"},
		"/api/v1/cli/attachment":                 {"get"},
		"/api/v1/cli/collections":                {"get", "post"},
		"/api/v1/cli/collections/{name}":         {"delete"},
		"/api/v1/cli/collections/{name}/sources": {"patch", "delete"},
		"/api/v1/cli/collection":                 {"get"},
		"/api/v1/cli/identities":                 {"get", "post", "delete"},
		"/api/v1/cli/deduplicate/plan":           {"post"},
		"/api/v1/cli/delete-deduped":             {"post"},
		"/api/v1/cli/delete-deduped/plan":        {"post"},
		"/api/v1/cli/rebuild-fts":                {"post"},
		"/api/v1/messages":                       {"get"},
		"/api/v1/messages/{id}":                  {"get"},
		"/api/v1/messages/{id}/inline":           {"get"},
		"/api/v1/search":                         {"get"},
		"/api/v1/query":                          {"post"},
		"/api/v1/aggregates":                     {"get"},
		"/api/v1/aggregates/sub":                 {"get"},
		"/api/v1/messages/filter":                {"get"},
		"/api/v1/messages/gmail-ids":             {"get"},
		"/api/v1/stats/total":                    {"get"},
		"/api/v1/search/fast":                    {"get"},
		"/api/v1/search/deep":                    {"get"},
		"/api/v1/accounts":                       {"get", "post"},
		"/api/v1/sources/status":                 {"get"},
		"/api/v1/sync/{account}":                 {"post"},
		"/api/v1/scheduler/status":               {"get"},
		"/api/v1/auth/token/{email}":             {"post"},
	}

	for path, methods := range expected {
		operations, ok := doc.Paths[path]
		require.True(ok, "path %s present", path)
		for _, method := range methods {
			require.Contains(operations, method, "%s %s present", method, path)
		}
	}

	successResponses := map[string]map[string][]string{
		"/api/v1/accounts": {
			"post": {"200", "201"},
		},
		"/api/v1/sync/{account}": {
			"post": {"202"},
		},
		"/api/v1/auth/token/{email}": {
			"post": {"201"},
		},
	}
	for path, methods := range successResponses {
		operations := doc.Paths[path]
		for method, statuses := range methods {
			op, ok := operations[method].(map[string]any)
			require.True(ok, "%s %s operation object", method, path)
			responses, ok := op["responses"].(map[string]any)
			require.True(ok, "%s %s responses object", method, path)
			for _, status := range statuses {
				require.Contains(responses, status, "%s %s success status", method, path)
			}
		}
	}

	collectionCreate, ok := doc.Paths["/api/v1/cli/collections"]["post"].(map[string]any)
	require.True(ok, "collection create operation object")
	responses, ok := collectionCreate["responses"].(map[string]any)
	require.True(ok, "collection create responses object")
	require.Contains(responses, "404", "collection create unresolved account response")
}

func TestCLIInitDBRunsStartupMigrationsAndReturnsStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	cfg := &config.Config{
		Identity: config.IdentityConfig{Addresses: []string{"alice@example.com"}},
		Server:   config.ServerConfig{APIPort: 8080},
	}
	srv := NewServer(cfg, st, nil, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/init-db", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusOK, resp.Code, "init-db status: %s", resp.Body.String())

	var out cliInitDBResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&out), "decode response")
	assert.Contains(out.Notice, "legacy [identity] config", "migration notice")
	assert.Equal(int64(0), out.Stats.TotalMessages, "messages")
	assert.Equal(int64(0), out.Stats.TotalThreads, "threads")
	assert.Equal(int64(0), out.Stats.TotalAccounts, "accounts")
}

func TestCLICacheStatsReportsMissingCacheThroughDaemon(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	cfg := &config.Config{
		Data:   config.DataConfig{DataDir: t.TempDir()},
		Server: config.ServerConfig{APIPort: 8080},
	}
	srv := NewServer(cfg, st, nil, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/cache-stats", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusOK, resp.Code, "cache-stats status: %s", resp.Body.String())
	var out cliCacheStatsResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&out), "decode response")
	assert.Equal("no_cache_files", out.Status, "status")
}

func TestHandleCLIBuildCacheStreamsOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var gotFullRebuild bool
	st := &mockStore{
		buildCacheFunc: func(_ context.Context, fullRebuild bool, emit func(CLICacheBuildEvent) error) error {
			gotFullRebuild = fullRebuild
			require.NoError(emit(CLICacheBuildEvent{Type: "stdout", Data: "Building cache...\n"}), "emit stdout")
			require.NoError(emit(CLICacheBuildEvent{Type: "stderr", Data: "Warning: using CSV fallback\n"}), "emit stderr")
			return nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	resp := servePOSTTestRequest(srv, "/api/v1/cli/build-cache?full_rebuild=true")

	requireNDJSONResponse(t, resp)
	assert.True(gotFullRebuild, "full rebuild")

	events := decodeNDJSONEvents[CLICacheBuildEvent](t, resp.Body)
	require.Len(events, 3, "events")
	assert.Equal(CLICacheBuildEvent{Type: "stdout", Data: "Building cache...\n"}, events[0], "stdout event")
	assert.Equal(CLICacheBuildEvent{Type: "stderr", Data: "Warning: using CSV fallback\n"}, events[1], "stderr event")
	assert.Equal(CLICacheBuildEvent{Type: cliStreamEventTypeComplete}, events[2], "complete event")
}

func TestHandleCLISyncFullStreamsOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var gotReq CLISyncRequest
	st := &mockStore{
		syncFunc: func(_ context.Context, req CLISyncRequest, emit func(CLISyncEvent) error) error {
			gotReq = req
			require.NoError(emit(CLISyncEvent{Type: "stdout", Data: "Starting full sync\n"}), "emit stdout")
			require.NoError(emit(CLISyncEvent{Type: "stderr", Data: "sync warning\n"}), "emit stderr")
			return nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	resp := servePOSTTestRequest(
		srv,
		"/api/v1/cli/sync-full?email=alice@example.com&query=from%3Abob&after=2024-01-01&before=2024-12-31&limit=25&noresume=true",
	)

	requireNDJSONResponse(t, resp)
	assert.Equal(CLISyncRequest{
		Full:     true,
		Email:    "alice@example.com",
		Query:    "from:bob",
		NoResume: true,
		Before:   "2024-12-31",
		After:    "2024-01-01",
		Limit:    25,
	}, gotReq, "sync request")

	events := decodeNDJSONEvents[CLISyncEvent](t, resp.Body)
	require.Len(events, 3, "events")
	assert.Equal(CLISyncEvent{Type: "stdout", Data: "Starting full sync\n"}, events[0], "stdout event")
	assert.Equal(CLISyncEvent{Type: "stderr", Data: "sync warning\n"}, events[1], "stderr event")
	assert.Equal(CLISyncEvent{Type: cliStreamEventTypeComplete}, events[2], "complete event")
}

func TestHandleCLIVerifyStreamsOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var gotReq CLIVerifyRequest
	st := &mockStore{
		verifyFunc: func(_ context.Context, req CLIVerifyRequest, emit func(CLIVerifyEvent) error) error {
			gotReq = req
			require.NoError(emit(CLIVerifyEvent{Type: "stdout", Data: "Verifying archive\n"}), "emit stdout")
			require.NoError(emit(CLIVerifyEvent{Type: "stderr", Data: "verify warning\n"}), "emit stderr")
			return nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	resp := servePOSTTestRequest(
		srv,
		"/api/v1/cli/verify?email=alice@example.com&sample=25&skip_db_check=true&json=true",
	)

	requireNDJSONResponse(t, resp)
	assert.Equal(CLIVerifyRequest{
		Email:       "alice@example.com",
		SampleSize:  25,
		SkipDBCheck: true,
		JSON:        true,
	}, gotReq, "verify request")

	events := decodeNDJSONEvents[CLIVerifyEvent](t, resp.Body)
	require.Len(events, 3, "events")
	assert.Equal(CLIVerifyEvent{Type: "stdout", Data: "Verifying archive\n"}, events[0], "stdout event")
	assert.Equal(CLIVerifyEvent{Type: "stderr", Data: "verify warning\n"}, events[1], "stderr event")
	assert.Equal(CLIVerifyEvent{Type: cliStreamEventTypeComplete}, events[2], "complete event")
}

func TestHandleCLIRepairEncodingStreamsOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := &mockStore{
		repairFunc: func(_ context.Context, emit func(CLIRepairEncodingEvent) error) error {
			require.NoError(emit(CLIRepairEncodingEvent{Type: "stdout", Data: "Scanning messages\n"}), "emit stdout")
			require.NoError(emit(CLIRepairEncodingEvent{Type: "stderr", Data: "repair warning\n"}), "emit stderr")
			return nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	resp := servePOSTTestRequest(srv, "/api/v1/cli/repair-encoding")

	requireNDJSONResponse(t, resp)
	events := decodeNDJSONEvents[CLIRepairEncodingEvent](t, resp.Body)
	require.Len(events, 3, "events")
	assert.Equal(CLIRepairEncodingEvent{Type: "stdout", Data: "Scanning messages\n"}, events[0], "stdout event")
	assert.Equal(CLIRepairEncodingEvent{Type: "stderr", Data: "repair warning\n"}, events[1], "stderr event")
	assert.Equal(CLIRepairEncodingEvent{Type: cliStreamEventTypeComplete}, events[2], "complete event")
}

func TestHandleCLIRunStreamsGenericCommandOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var gotReq CLIRunRequest
	st := &mockStore{
		runFunc: func(_ context.Context, req CLIRunRequest, emit func(CLIRunEvent) error) error {
			gotReq = req
			require.NoError(emit(CLIRunEvent{Type: "stdout", Data: "Removing account\n"}), "emit stdout")
			require.NoError(emit(CLIRunEvent{Type: "stderr", Data: "remove warning\n"}), "emit stderr")
			return nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	body := strings.NewReader(`{"args":["remove-account","alice@example.com","--yes"],"env":{"MSGVAULT_IMAP_PASSWORD":"secret"},"cwd":"/caller"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	requireNDJSONResponse(t, resp)
	assert.Equal(CLIRunRequest{
		Args: []string{"remove-account", "alice@example.com", "--yes"},
		Env:  map[string]string{"MSGVAULT_IMAP_PASSWORD": "secret"},
		Cwd:  "/caller",
	}, gotReq, "run request")

	events := decodeNDJSONEvents[CLIRunEvent](t, resp.Body)
	require.Len(events, 3, "events")
	assert.Equal(CLIRunEvent{Type: "stdout", Data: "Removing account\n"}, events[0], "stdout event")
	assert.Equal(CLIRunEvent{Type: "stderr", Data: "remove warning\n"}, events[1], "stderr event")
	assert.Equal(CLIRunEvent{Type: cliStreamEventTypeComplete}, events[2], "complete event")
}

func TestHandleCLIRunAllowsLegacyBuildEmbeddingsCommand(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var gotReq CLIRunRequest
	st := &mockStore{
		runFunc: func(_ context.Context, req CLIRunRequest, emit func(CLIRunEvent) error) error {
			gotReq = req
			require.NoError(emit(CLIRunEvent{Type: "stdout", Data: "Building generation 2\n"}), "emit stdout")
			return nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	body := strings.NewReader(`{"args":["build-embeddings","--backstop"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	requireNDJSONResponse(t, resp)
	assert.Equal(CLIRunRequest{
		Args: []string{"build-embeddings", "--backstop"},
	}, gotReq, "run request")

	events := decodeNDJSONEvents[CLIRunEvent](t, resp.Body)
	require.Len(events, 2, "events")
	assert.Equal(CLIRunEvent{Type: "stdout", Data: "Building generation 2\n"}, events[0], "stdout event")
	assert.Equal(CLIRunEvent{Type: cliStreamEventTypeComplete}, events[1], "complete event")
}

func TestHandleCLIRunAllowsLogsCommand(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var gotReq CLIRunRequest
	st := &mockStore{
		runFunc: func(_ context.Context, req CLIRunRequest, emit func(CLIRunEvent) error) error {
			gotReq = req
			require.NoError(emit(CLIRunEvent{Type: "stdout", Data: "recent log\n"}), "emit stdout")
			return nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	body := strings.NewReader(`{"args":["logs","--lines=10"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	requireNDJSONResponse(t, resp)
	assert.Equal(CLIRunRequest{
		Args: []string{"logs", "--lines=10"},
	}, gotReq, "run request")

	events := decodeNDJSONEvents[CLIRunEvent](t, resp.Body)
	require.Len(events, 2, "events")
	assert.Equal(CLIRunEvent{Type: "stdout", Data: "recent log\n"}, events[0], "stdout event")
	assert.Equal(CLIRunEvent{Type: cliStreamEventTypeComplete}, events[1], "complete event")
}

func TestHandleCLIRunBypassesStandardRequestTimeout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	canceled := false
	st := &mockStore{
		runFunc: func(ctx context.Context, req CLIRunRequest, emit func(CLIRunEvent) error) error {
			assert.Equal([]string{"deduplicate", "--dry-run"}, req.Args, "args")
			time.Sleep(40 * time.Millisecond)
			if err := ctx.Err(); err != nil {
				canceled = true
				return err
			}
			return emit(CLIRunEvent{Type: cliStreamEventTypeComplete})
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:         &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:          st,
		Logger:         testLogger(),
		RequestTimeout: 5 * time.Millisecond,
	})

	body := strings.NewReader(`{"args":["deduplicate","--dry-run"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusOK, resp.Code, "status: %s", resp.Body.String())
	assert.False(canceled, "cli runner context should not use the standard request timeout")
	assert.Contains(resp.Body.String(), `"type":"complete"`, "body")
}

func TestHandleQueryEnforcesQueryTimeout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	started := make(chan struct{})
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Logger: testLogger(),
		SQLQueryRunner: func(ctx context.Context, _ string) (*query.QueryResult, error) {
			close(started)
			<-ctx.Done() // simulate a runaway query that only stops on cancellation
			return nil, ctx.Err()
		},
	})
	// Test seam: shrink the query ceiling so the timeout fires immediately.
	srv.queryTimeout = 20 * time.Millisecond

	body := strings.NewReader(`{"sql":"SELECT 1"}`)
	req := httptest.NewRequest(http.MethodPost, queryEndpointPath, body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.Router().ServeHTTP(resp, req)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		require.FailNow("query runner never started")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow("request did not return after query timeout")
	}

	require.Equal(http.StatusServiceUnavailable, resp.Code, "body: %s", resp.Body.String())
	assert.Contains(resp.Body.String(), "query_timeout")
}

func TestHandleCLIRunRejectsDisallowedEnv(t *testing.T) {
	assert := assert.New(t)

	st := &mockStore{
		runFunc: func(context.Context, CLIRunRequest, func(CLIRunEvent) error) error {
			require.FailNow(t, "disallowed env should not reach runner")
			return nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	body := strings.NewReader(`{"args":["add-imap"],"env":{"PATH":"/tmp/bin"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	assert.Equal(http.StatusBadRequest, resp.Code, "status: %s", resp.Body.String())
	assert.Contains(resp.Body.String(), "env_not_allowed", "error body")
}

func TestHandleCLIAddCalendarPlanReturnsPrompt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var gotReq CLIAddCalendarPlanRequest
	st := &mockStore{
		planCalendarFunc: func(_ context.Context, req CLIAddCalendarPlanRequest) (CLIAddCalendarPlanResponse, error) {
			gotReq = req
			return CLIAddCalendarPlanResponse{
				NeedsScopeEscalation: true,
				Headline:             "CALENDAR ACCESS REQUIRED",
				BodyLines:            []string{"Calendar sync needs read-only Calendar access."},
				CancelHint:           "Cancelled. Calendar was not added.",
			}, nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	body := strings.NewReader(`{"email":"alice@example.com","oauth_app":"acme","oauth_app_explicit":true,"headless":false}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/add-calendar/plan", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusOK, resp.Code, "status: %s", resp.Body.String())
	assert.Equal(CLIAddCalendarPlanRequest{
		Email:            "alice@example.com",
		OAuthApp:         "acme",
		OAuthAppExplicit: true,
		Headless:         false,
	}, gotReq, "plan request")

	var got CLIAddCalendarPlanResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&got), "decode response")
	assert.True(got.NeedsScopeEscalation, "needs scope escalation")
	assert.Equal("CALENDAR ACCESS REQUIRED", got.Headline, "headline")
	assert.Equal([]string{"Calendar sync needs read-only Calendar access."}, got.BodyLines, "body lines")
	assert.Equal("Cancelled. Calendar was not added.", got.CancelHint, "cancel hint")
}

func TestHandleCLIEmbeddingsPlanReturnsPrompt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var gotReq CLIEmbeddingsPlanRequest
	st := &mockStore{
		planEmbedsFunc: func(_ context.Context, req CLIEmbeddingsPlanRequest) (CLIEmbeddingsPlanResponse, error) {
			gotReq = req
			return CLIEmbeddingsPlanResponse{
				NeedsConfirmation: true,
				Prompt:            "Activate generation 2 (fp)? ",
			}, nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	body := strings.NewReader(`{"operation":"activate","generation_id":2,"force":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/embeddings/plan", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusOK, resp.Code, "status: %s", resp.Body.String())
	assert.Equal(CLIEmbeddingsPlanRequest{
		Operation:    "activate",
		GenerationID: 2,
		Force:        true,
	}, gotReq, "plan request")

	var got CLIEmbeddingsPlanResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&got), "decode response")
	assert.True(got.NeedsConfirmation, "needs confirmation")
	assert.Equal("Activate generation 2 (fp)? ", got.Prompt, "prompt")
}

func TestHandleCLIDeleteStagedPlanReturnsPrompt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var gotReq CLIDeleteStagedPlanRequest
	st := &mockStore{
		planDeleteFunc: func(_ context.Context, req CLIDeleteStagedPlanRequest) (CLIDeleteStagedPlanResponse, error) {
			gotReq = req
			return CLIDeleteStagedPlanResponse{
				Stdout:                    "Deletion Summary:\n",
				NeedsExecution:            true,
				NeedsConfirmation:         true,
				ConfirmationMode:          "trash",
				PlannedBatchIDs:           []string{"batch-123"},
				PlanFingerprint:           "fp-handler",
				NeedsScopeEscalation:      true,
				ScopeEscalationHeadline:   "PERMISSION UPGRADE REQUIRED",
				ScopeEscalationBodyLines:  []string{"Batch deletion requires elevated Gmail permissions."},
				ScopeEscalationCancelHint: "Cancelled.",
				RemoteDeleteEnvVar:        "MSGVAULT_ENABLE_REMOTE_DELETE",
			}, nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	body := strings.NewReader(`{"batch_id":"batch-123","permanent":false,"yes":false,"dry_run":false,"list":false,"account":"alice@example.com","remote_delete_enabled":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/delete-staged/plan", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusOK, resp.Code, "status: %s", resp.Body.String())
	assert.Equal(CLIDeleteStagedPlanRequest{
		BatchID:             "batch-123",
		Account:             "alice@example.com",
		RemoteDeleteEnabled: true,
	}, gotReq, "plan request")

	var got CLIDeleteStagedPlanResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&got), "decode response")
	assert.Equal("Deletion Summary:\n", got.Stdout, "stdout")
	assert.True(got.NeedsExecution, "needs execution")
	assert.True(got.NeedsConfirmation, "needs confirmation")
	assert.Equal("trash", got.ConfirmationMode, "confirmation mode")
	assert.Equal([]string{"batch-123"}, got.PlannedBatchIDs, "planned batch ids")
	assert.Equal("fp-handler", got.PlanFingerprint, "plan fingerprint")
	assert.True(got.NeedsScopeEscalation, "needs scope escalation")
	assert.Equal("PERMISSION UPGRADE REQUIRED", got.ScopeEscalationHeadline, "scope headline")
	assert.Equal([]string{"Batch deletion requires elevated Gmail permissions."}, got.ScopeEscalationBodyLines, "scope body")
	assert.Equal("Cancelled.", got.ScopeEscalationCancelHint, "scope cancel hint")
	assert.Equal("MSGVAULT_ENABLE_REMOTE_DELETE", got.RemoteDeleteEnvVar, "remote delete env")
}

func TestHandleCLIDeduplicatePlanReturnsItems(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var gotReq CLIDeduplicatePlanRequest
	st := &mockStore{
		planDedupFunc: func(_ context.Context, req CLIDeduplicatePlanRequest) (CLIDeduplicatePlanResponse, error) {
			gotReq = req
			return CLIDeduplicatePlanResponse{
				PrefixStdout: "No --account specified; deduping each source independently.\n\n",
				Items: []CLIDeduplicatePlanItem{
					{
						SourceID:          42,
						ScopeLabel:        "alice@example.com",
						Stdout:            "Duplicate groups found: 1\n",
						DuplicateMessages: 2,
						BackfilledCount:   3,
						PlanFingerprint:   "fp-dedup",
						NeedsConfirmation: true,
					},
				},
			}, nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	body := strings.NewReader(`{"account":"alice@example.com","prefer":"gmail,mbox","content_hash":true,"delete_dups_from_source_server":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/deduplicate/plan", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusOK, resp.Code, "status: %s", resp.Body.String())
	assert.Equal(CLIDeduplicatePlanRequest{
		Account:                    "alice@example.com",
		Prefer:                     "gmail,mbox",
		ContentHash:                true,
		DeleteDupsFromSourceServer: true,
	}, gotReq, "plan request")

	var got CLIDeduplicatePlanResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&got), "decode response")
	assert.Equal("No --account specified; deduping each source independently.\n\n", got.PrefixStdout, "prefix")
	require.Len(got.Items, 1, "items")
	assert.Equal(int64(42), got.Items[0].SourceID, "source id")
	assert.Equal("fp-dedup", got.Items[0].PlanFingerprint, "fingerprint")
	assert.True(got.Items[0].NeedsConfirmation, "needs confirmation")
}

func TestHandleCLIDeduplicatePlanRequestErrorUsesAPIEnvelope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := &mockStore{
		planDedupFunc: func(context.Context, CLIDeduplicatePlanRequest) (CLIDeduplicatePlanResponse, error) {
			return CLIDeduplicatePlanResponse{}, opserr.NotFound(errors.New(`no account found for "missing@example.com"`))
		},
	}
	srv := newCLIHandlerTestServer(st)

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/deduplicate/plan",
		strings.NewReader(`{"account":"missing@example.com"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assert.Equal(http.StatusBadRequest, resp.Code, "status: %s", resp.Body.String())
	assert.Contains(resp.Header().Get("Content-Type"), "application/json", "content type")
	assert.NotContains(resp.Header().Get("Content-Type"), "application/problem+json", "problem details must not leak")

	var out ErrorResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&out), "decode error response")
	assert.Equal("deduplicate_plan_failed", out.Error, "error code")
	assert.Equal(`no account found for "missing@example.com"`, out.Message, "error message")
}

func TestHandleCLIDeduplicatePlanInvalidCollectionUsesAPIEnvelope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := &mockStore{
		planDedupFunc: func(context.Context, CLIDeduplicatePlanRequest) (CLIDeduplicatePlanResponse, error) {
			return CLIDeduplicatePlanResponse{}, opserr.Invalid(errors.New(`--collection "calendars" has no member accounts`))
		},
	}
	srv := newCLIHandlerTestServer(st)

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/deduplicate/plan",
		strings.NewReader(`{"collection":"calendars"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assert.Equal(http.StatusBadRequest, resp.Code, "status: %s", resp.Body.String())
	assert.Contains(resp.Header().Get("Content-Type"), "application/json", "content type")

	var out ErrorResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&out), "decode error response")
	assert.Equal("deduplicate_plan_failed", out.Error, "error code")
	assert.Equal(`--collection "calendars" has no member accounts`, out.Message, "error message")
}

func TestHandleCLIRunRejectsDisallowedCommand(t *testing.T) {
	assert := assert.New(t)

	st := &mockStore{
		runFunc: func(context.Context, CLIRunRequest, func(CLIRunEvent) error) error {
			require.FailNow(t, "disallowed command should not reach runner")
			return nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	body := strings.NewReader(`{"args":["serve","restart"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	assert.Equal(http.StatusBadRequest, resp.Code, "status")
	assert.Contains(resp.Body.String(), "command is not allowed", "error")
}

func TestHandleCLIRunBackupSubcommandAdmission(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		allowed bool
	}{
		{"backup create allowed", []string{"backup", "create"}, true},
		{"backup init rejected", []string{"backup", "init"}, false},
		{"backup verify rejected", []string{"backup", "verify"}, false},
		{"backup with no subcommand rejected", []string{"backup"}, false},
		{"backup unknown subcommand rejected", []string{"backup", "restore"}, false},
		{"logs still allowed", []string{"logs"}, true},
		{"remove-account still allowed", []string{"remove-account", "alice@example.com", "--yes"}, true},
		{"pack-attachments allowed", []string{"pack-attachments"}, true},
		{"repack-attachments allowed", []string{"repack-attachments"}, true},
		{"unpack-attachments rejected", []string{"unpack-attachments"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st := &mockStore{
				runFunc: func(_ context.Context, _ CLIRunRequest, emit func(CLIRunEvent) error) error {
					return emit(CLIRunEvent{Type: cliStreamEventTypeComplete})
				},
			}
			srv := newCLIHandlerTestServer(st)

			body, err := json.Marshal(CLIRunRequest{Args: tc.args})
			require.NoError(err, "marshal request")
			req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)

			if tc.allowed {
				assert.Equal(http.StatusOK, resp.Code, "status: %s", resp.Body.String())
			} else {
				assert.Equal(http.StatusBadRequest, resp.Code, "status: %s", resp.Body.String())
				assert.Contains(resp.Body.String(), "command_not_allowed", "error body")
			}
		})
	}
}

func TestCLIDeleteDedupedPlansAndExecutesThroughDaemon(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	keepA := createAPIDedupMessage(t, f, "keep-a", "dedup-api-a")
	dropA := createAPIDedupMessage(t, f, "drop-a", "dedup-api-a")
	keepB := createAPIDedupMessage(t, f, "keep-b", "dedup-api-b")
	dropB := createAPIDedupMessage(t, f, "drop-b", "dedup-api-b")
	_, err := f.Store.MergeDuplicates(keepA, []int64{dropA}, "batch-a")
	require.NoError(err, "merge batch-a")
	_, err = f.Store.MergeDuplicates(keepB, []int64{dropB}, "batch-b")
	require.NoError(err, "merge batch-b")

	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, f.Store, nil, testLogger())

	planReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/delete-deduped/plan",
		strings.NewReader(`{"batch_ids":["batch-a","batch-b"]}`),
	)
	planReq.Header.Set("Content-Type", "application/json")
	planResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(planResp, planReq)

	require.Equal(http.StatusOK, planResp.Code, "plan status: %s", planResp.Body.String())
	var plan cliDeleteDedupedPlanResponse
	require.NoError(json.NewDecoder(planResp.Body).Decode(&plan), "decode plan")
	assert.Equal(int64(2), plan.Total, "plan total")
	assert.Equal(int64(2), plan.BatchCount, "plan batch count")
	require.Len(plan.Batches, 2, "plan batches")
	assert.Equal("batch-a", plan.Batches[0].ID, "batch-a id")
	assert.Equal(int64(1), plan.Batches[0].Count, "batch-a count")
	assert.Equal("batch-b", plan.Batches[1].ID, "batch-b id")
	assert.Equal(int64(1), plan.Batches[1].Count, "batch-b count")

	executeReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/delete-deduped",
		strings.NewReader(`{
			"batch_ids":["batch-a","batch-b"],
			"no_backup": true,
			"expected_total": 2,
			"expected_batch_count": 2,
			"expected_batches": [
				{"id":"batch-a", "count": 1},
				{"id":"batch-b", "count": 1}
			]
		}`),
	)
	executeReq.Header.Set("Content-Type", "application/json")
	executeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(executeResp, executeReq)

	require.Equal(http.StatusOK, executeResp.Code, "execute status: %s", executeResp.Body.String())
	var executed cliDeleteDedupedExecuteResponse
	require.NoError(json.NewDecoder(executeResp.Body).Decode(&executed), "decode execute")
	assert.Equal(int64(2), executed.Deleted, "deleted")
	assert.Equal(int64(2), executed.BatchCount, "execute batch count")

	var count int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT COUNT(*) FROM messages WHERE id IN (?, ?)"),
		dropA,
		dropB,
	).Scan(&count)
	require.NoError(err, "count deleted rows")
	assert.Equal(0, count, "deduped rows should be deleted")
}

func TestCLIDeleteDedupedRejectsChangedBatchPlan(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	keepA := createAPIDedupMessage(t, f, "keep-a", "dedup-api-change-a")
	dropA := createAPIDedupMessage(t, f, "drop-a", "dedup-api-change-a")
	keepB := createAPIDedupMessage(t, f, "keep-b", "dedup-api-change-b")
	dropB := createAPIDedupMessage(t, f, "drop-b", "dedup-api-change-b")
	_, err := f.Store.MergeDuplicates(keepA, []int64{dropA}, "batch-a")
	require.NoError(err, "merge batch-a")
	_, err = f.Store.MergeDuplicates(keepB, []int64{dropB}, "batch-b")
	require.NoError(err, "merge batch-b")

	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, f.Store, nil, testLogger())

	executeReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/delete-deduped",
		strings.NewReader(`{
			"batch_ids":["batch-a","batch-b"],
			"no_backup": true,
			"expected_total": 2,
			"expected_batch_count": 2,
			"expected_batches": [
				{"id":"batch-a", "count": 2},
				{"id":"batch-b", "count": 0}
			]
		}`),
	)
	executeReq.Header.Set("Content-Type", "application/json")
	executeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(executeResp, executeReq)

	assert.Equal(http.StatusConflict, executeResp.Code, "execute status: %s", executeResp.Body.String())

	var count int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT COUNT(*) FROM messages WHERE id IN (?, ?)"),
		dropA,
		dropB,
	).Scan(&count)
	require.NoError(err, "count rows after rejected execute")
	assert.Equal(2, count, "deduped rows should remain when plan changes")
}

func TestCLIDeleteDedupedRequiresExpectedBatchesForBatchExecute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	keepA := createAPIDedupMessage(t, f, "keep-a", "dedup-api-require-a")
	dropA := createAPIDedupMessage(t, f, "drop-a", "dedup-api-require-a")
	keepB := createAPIDedupMessage(t, f, "keep-b", "dedup-api-require-b")
	dropB := createAPIDedupMessage(t, f, "drop-b", "dedup-api-require-b")
	_, err := f.Store.MergeDuplicates(keepA, []int64{dropA}, "batch-a")
	require.NoError(err, "merge batch-a")
	_, err = f.Store.MergeDuplicates(keepB, []int64{dropB}, "batch-b")
	require.NoError(err, "merge batch-b")

	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, f.Store, nil, testLogger())

	executeReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/delete-deduped",
		strings.NewReader(`{
			"batch_ids":["batch-a","batch-b"],
			"no_backup": true,
			"expected_total": 2,
			"expected_batch_count": 2
		}`),
	)
	executeReq.Header.Set("Content-Type", "application/json")
	executeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(executeResp, executeReq)

	assert.Equal(http.StatusBadRequest, executeResp.Code, "execute status: %s", executeResp.Body.String())

	var count int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT COUNT(*) FROM messages WHERE id IN (?, ?)"),
		dropA,
		dropB,
	).Scan(&count)
	require.NoError(err, "count rows after rejected execute")
	assert.Equal(2, count, "deduped rows should remain without expected batch counts")
}

func createAPIDedupMessage(t *testing.T, f *storetest.Fixture, sourceMessageID, rfc822ID string) int64 {
	t.Helper()
	id, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: sourceMessageID,
		RFC822MessageID: sql.NullString{String: rfc822ID, Valid: rfc822ID != ""},
		MessageType:     "email",
		SizeEstimate:    1000,
	})
	require.NoError(t, err, "upsert dedup test message")
	return id
}

func TestHandleCLIIdentityAddPreservesErrorEnvelope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/identities",
		strings.NewReader(`{"identifier":"extra@example.com","signal":"manual"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assert.Equal(http.StatusBadRequest, resp.Code, "add error status: %s", resp.Body.String())
	assert.Contains(resp.Header().Get("Content-Type"), "application/json", "content type")
	assert.NotContains(resp.Header().Get("Content-Type"), "application/problem+json", "problem details must not leak")

	var out ErrorResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&out), "decode error response")
	assert.Equal("invalid_identity", out.Error, "error code")
	assert.Equal("account is required", out.Message, "error message")
}

func TestHandleCLIIdentityAddMissingAccountUsesNotFoundCode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/identities",
		strings.NewReader(`{
			"account":"missing@example.com",
			"identifier":"extra@example.com",
			"signal":"manual"
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assert.Equal(http.StatusBadRequest, resp.Code, "add error status: %s", resp.Body.String())
	var out ErrorResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&out), "decode error response")
	assert.Equal("identity_not_found", out.Error, "error code")
	assert.Contains(out.Message, `no account found for "missing@example.com"`, "error message")
}

func TestOperationErrorPoliciesDocumentNotFoundStatus(t *testing.T) {
	assert := assert.New(t)
	srv := &Server{logger: testLogger()}

	collectionErr := srv.operationError(
		opserr.Wrap(opserr.KindNotFound, errors.New("missing collection")),
		collectionOperationErrorPolicy,
		"collection failed",
	)
	assert.Equal(http.StatusNotFound, collectionErr.GetStatus(), "collection not found status")
	assert.Equal("not_found", collectionErr.ErrorResponse.Error, "collection error code")
	assert.Equal("missing collection", collectionErr.Message, "collection message")

	identityErr := srv.operationError(
		opserr.Wrap(opserr.KindNotFound, errors.New("missing identity")),
		identityOperationErrorPolicy,
		"identity failed",
	)
	assert.Equal(http.StatusBadRequest, identityErr.GetStatus(), "identity not found status")
	assert.Equal("identity_not_found", identityErr.ErrorResponse.Error, "identity error code")
	assert.Equal("missing identity", identityErr.Message, "identity message")

	scopeErr := srv.operationError(
		opserr.Wrap(opserr.KindNotFound, errors.New("missing scope")),
		scopeOperationErrorPolicy,
		"scope failed",
	)
	assert.Equal(http.StatusBadRequest, scopeErr.GetStatus(), "scope not found status")
	assert.Equal("invalid_scope", scopeErr.ErrorResponse.Error, "scope error code")
	assert.Equal("missing scope", scopeErr.Message, "scope message")
}

func TestResolveCLIStatsScopeRejectsMutuallyExclusiveScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	_, err := resolveCLIStatsScope(st, "alice@example.com", "team")

	require.Error(err, "resolveCLIStatsScope")
	assert.Equal(opserr.KindInvalid, opserr.KindOf(err), "error kind")
	assert.Equal("account and collection are mutually exclusive", err.Error(), "error message")
}

func TestHandleCLISearchCollectionScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	defer func() { _ = engine.Close() }()
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	_, err = st.CreateCollection("Important", "important mail", []int64{src.ID})
	require.NoError(err, "CreateCollection")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?collection=Important&limit=10", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp struct {
		Results          []json.RawMessage `json:"results"`
		ScopeLabel       string            `json:"scope_label"`
		ScopeSourceCount int               `json:"scope_source_count"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal("Important", resp.ScopeLabel, "scope label")
	assert.Equal(1, resp.ScopeSourceCount, "scope source count")
	assert.Empty(resp.Results, "results")
}

// TestHandleCLISearchDoesNotBlockOnIndexBuild is the core cold-start UX
// contract: a search must return results immediately even while the FTS
// completeness probe (a minute on a large archive) or a backfill is running,
// reporting the background work's state instead of waiting on it.
func TestHandleCLISearchDoesNotBlockOnIndexBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	probeRelease := make(chan struct{})
	backfillEntered := make(chan struct{})
	backfillRelease := make(chan struct{})
	st := &mockStore{
		needsFTSBackfillFunc: func() bool {
			<-probeRelease // closed channel unblocks both probe calls
			return true
		},
		backfillFTSFunc: func(func(done, total int64)) (int64, error) {
			close(backfillEntered)
			<-backfillRelease
			return 12, nil
		},
	}
	engine := &querytest.MockEngine{
		SearchFunc: func(ctx context.Context, _ *search.Query, _, _ int) ([]query.MessageSummary, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []query.MessageSummary{{ID: 1, Subject: "match"}}, nil
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:         &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:          st,
		Engine:         engine,
		Logger:         testLogger(),
		RequestTimeout: 5 * time.Millisecond,
	})

	searchIndexState := func() string {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=hello&limit=10", nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		require.Equal(http.StatusOK, w.Code, "status: %s", w.Body.String())
		var resp struct {
			Results []struct {
				Subject string `json:"subject"`
			} `json:"results"`
			IndexBuilt bool   `json:"index_built"`
			IndexState string `json:"index_state"`
		}
		require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
		require.Len(resp.Results, 1, "results")
		assert.Equal("match", resp.Results[0].Subject, "subject")
		assert.False(resp.IndexBuilt, "index_built is a pre-0.18 synchronous-build field")
		return resp.IndexState
	}

	// The full probe is blocked and the quick tail-check found nothing, so
	// the first search answers instantly and quietly.
	assert.Equal("checking", searchIndexState(), "state while the probe runs")

	close(probeRelease)
	select {
	case <-backfillEntered:
	case <-time.After(time.Second):
		require.FailNow("background worker never reached the backfill")
	}
	assert.Equal("building", searchIndexState(), "state while the backfill runs")

	close(backfillRelease)
	require.Eventually(func() bool { return srv.ftsIndexComplete.Load() },
		time.Second, 5*time.Millisecond, "backfill completion must set the memo flag")
	assert.Empty(searchIndexState(), "state once the index is complete")
}

// TestHandleCLISearchProbeDiscardsResultStaleAfterRebuild reproduces the
// probe/rebuild race (roborev finding on f91f6cb): the ensure worker's
// completeness probe runs outside the operation gate, so a rebuild-fts can
// invalidate the index while the probe scans the pre-rebuild state. A
// "complete" observation from before the rebuild must be discarded — here
// the rebuild fails, so re-memoizing complete=true would make later searches
// trust an index the rebuild left partial.
func TestHandleCLISearchProbeDiscardsResultStaleAfterRebuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	probeEntered := make(chan struct{})
	releaseProbe := make(chan struct{})
	st := &mockStore{
		needsFTSBackfillFunc: func() bool {
			close(probeEntered)
			<-releaseProbe
			return false // observed the PRE-rebuild, complete index
		},
		rebuildFTSFunc: func(func(done, total int64)) (int64, error) {
			return 0, errors.New("rebuild exploded mid-batch")
		},
	}
	engine := &querytest.MockEngine{
		SearchFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
			return nil, nil
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})

	// First search spawns the ensure worker, which blocks inside the probe.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=hello", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(http.StatusOK, w.Code, "search status: %s", w.Body.String())
	select {
	case <-probeEntered:
	case <-time.After(time.Second):
		require.FailNow("the search never spawned the FTS completeness probe")
	}

	// A rebuild starts and fails while the probe is still scanning.
	resp := servePOSTTestRequest(srv, "/api/v1/cli/rebuild-fts")
	requireNDJSONResponse(t, resp)
	_ = decodeNDJSONEvents[cliRebuildFTSEvent](t, resp.Body)
	require.False(srv.ftsIndexComplete.Load(),
		"precondition: a failed rebuild leaves the completeness flag cleared")

	// The probe finishes with its pre-rebuild observation; the worker must
	// discard it rather than re-memoize complete=true over the failed rebuild.
	close(releaseProbe)
	require.Eventually(func() bool { return !srv.ftsEnsureRunning.Load() },
		time.Second, 5*time.Millisecond, "ensure worker must finish")
	assert.False(srv.ftsIndexComplete.Load(),
		"a probe result observed before a rebuild must not be memoized")
}

// TestHandleCLISearchProbeRefusesMemoizeDuringRebuild covers the second
// probe/rebuild interleaving (roborev finding on a7752aa): the ensure worker
// starts AFTER a rebuild is already mid-flight (generation bumped to odd),
// and its probe's read snapshot may still predate the rebuild's index clear.
// A "complete" observation under an odd generation must not be memoized —
// here the rebuild is still running when the probe finishes, then fails.
func TestHandleCLISearchProbeRefusesMemoizeDuringRebuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	rebuildEntered := make(chan struct{})
	releaseRebuild := make(chan struct{})
	st := &mockStore{
		needsFTSBackfill: false, // probe sees the pre-clear "complete" index
		rebuildFTSFunc: func(func(done, total int64)) (int64, error) {
			close(rebuildEntered)
			<-releaseRebuild
			return 0, errors.New("rebuild exploded mid-batch")
		},
	}
	engine := &querytest.MockEngine{
		SearchFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
			return nil, nil
		},
	}
	srv := newCLIHandlerTestServer(st)
	srv.engine = engine

	rebuildDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rebuildDone <- servePOSTTestRequest(srv, "/api/v1/cli/rebuild-fts")
	}()
	select {
	case <-rebuildEntered:
	case <-time.After(time.Second):
		require.FailNow("rebuild never started")
	}

	// A search arrives while the rebuild is mid-flight; its ensure worker
	// probes the (snapshot-wise still complete) index.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=hello", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(http.StatusOK, w.Code, "search status: %s", w.Body.String())
	require.Eventually(func() bool { return !srv.ftsEnsureRunning.Load() },
		time.Second, 5*time.Millisecond, "ensure worker must finish")

	assert.False(srv.ftsIndexComplete.Load(),
		"a probe observation made while a rebuild is mid-flight must not be memoized")

	close(releaseRebuild)
	select {
	case resp := <-rebuildDone:
		requireNDJSONResponse(t, resp)
	case <-time.After(time.Second):
		require.FailNow("rebuild request did not finish")
	}
	assert.False(srv.ftsIndexComplete.Load(),
		"the failed rebuild must leave the completeness flag cleared")
}

// TestHandleCLISearchQuickCheckReportsBuildingImmediately verifies that when
// the cheap tail-check already knows the index is stale, the very first
// search reports index_state="building" — so the CLI warns about incomplete
// results right away instead of staying silent for the minutes the full
// completeness probe can take (roborev finding on 2328a4f).
func TestHandleCLISearchQuickCheckReportsBuildingImmediately(t *testing.T) {
	require := require.New(t)
	probeRelease := make(chan struct{})
	t.Cleanup(func() { close(probeRelease) })
	st := &mockStore{
		needsFTSBackfillQuick: true,
		needsFTSBackfillFunc: func() bool {
			<-probeRelease // full probe stays busy for the whole test
			return false
		},
	}
	engine := &querytest.MockEngine{
		SearchFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
			return nil, nil
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=hello", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(http.StatusOK, w.Code, "status: %s", w.Body.String())

	var resp struct {
		IndexState string `json:"index_state"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal(t, "building", resp.IndexState,
		"a stale tail must be reported as building on the first response")
}

func TestHandleCLISearchBackfillUsesOperationGate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	gate := NewSerialOperationGate()
	releaseGate, ok := gate.BeginWork()
	require.True(ok, "occupy operation gate")

	backfillStarted := make(chan struct{}, 1)
	st := &mockStore{
		needsFTSBackfill: true,
		backfillFTSFunc: func(func(done, total int64)) (int64, error) {
			backfillStarted <- struct{}{}
			return 1, nil
		},
	}
	engine := &querytest.MockEngine{
		SearchFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
			return []query.MessageSummary{{ID: 1, Subject: "match"}}, nil
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:         st,
		Engine:        engine,
		Logger:        testLogger(),
		OperationGate: gate,
	})

	// The search itself must not queue behind the held gate.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=hello&limit=10", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	assert.Equal(http.StatusOK, resp.Code, "status: %s", resp.Body.String())

	select {
	case <-backfillStarted:
		assert.Fail("backfill started while operation gate was occupied")
	case <-time.After(40 * time.Millisecond):
	}

	releaseGate()
	select {
	case <-backfillStarted:
	case <-time.After(500 * time.Millisecond):
		require.FailNow("backfill did not start after gate release")
	}
	require.Eventually(func() bool { return srv.ftsIndexComplete.Load() },
		time.Second, 5*time.Millisecond, "backfill completion must set the memo flag")
}

// TestHandleCLISearchMemoizesFTSComplete verifies that once the FTS index is
// confirmed complete, subsequent CLI searches do not re-run the expensive
// NeedsFTSBackfill probe (an anti-join that scans every message on a healthy
// index). This is the fix for the CLI-search-slow-vs-fast-search divergence.
func TestHandleCLISearchMemoizesFTSComplete(t *testing.T) {
	assert := assert.New(t)
	st := &mockStore{needsFTSBackfill: false}
	engine := &querytest.MockEngine{
		SearchFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
			return []query.MessageSummary{{ID: 1, Subject: "match"}}, nil
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})

	doSearch := func() {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=hello&limit=10", nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(http.StatusOK, w.Code, "status: %s", w.Body.String())
	}

	doSearch()
	// The probe runs in a background worker now; wait for it to confirm
	// completeness before checking that later searches skip it.
	require.Eventually(t, func() bool { return srv.ftsIndexComplete.Load() },
		time.Second, 5*time.Millisecond, "probe must confirm the index complete")
	doSearch()
	doSearch()

	assert.Equal(int32(1), st.needsFTSBackfillCalls.Load(),
		"NeedsFTSBackfill should be probed once, then memoized")
}

// TestHandleCLISearchReportsProbeInAuthenticatedHealth verifies that while
// the background worker spawned by the first search runs the expensive FTS
// completeness probe, authenticated /health tells clients what the daemon is
// doing instead of reporting it idle.
func TestHandleCLISearchReportsProbeInAuthenticatedHealth(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	probeEntered := make(chan struct{})
	releaseProbe := make(chan struct{})
	st := &mockStore{needsFTSBackfillFunc: func() bool {
		close(probeEntered)
		<-releaseProbe
		return false
	}}
	engine := &querytest.MockEngine{
		SearchFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
			return nil, nil
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080, APIKey: "secret-key"}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})

	// The first search spawns the probe worker and returns without waiting.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=hello", nil)
	req.Header.Set("X-Api-Key", "secret-key")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	assert.Equal(http.StatusOK, w.Code, "search status: %s", w.Body.String())

	select {
	case <-probeEntered:
	case <-time.After(time.Second):
		require.FailNow("the search never spawned the FTS completeness probe")
	}

	healthReq := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	healthReq.Header.Set("X-Api-Key", "secret-key")
	healthResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(healthResp, healthReq)
	require.Equal(http.StatusOK, healthResp.Code, "health status")
	var health HealthResponse
	require.NoError(json.Unmarshal(healthResp.Body.Bytes(), &health), "decode health body")
	require.NotNil(health.Operation, "health must report the running probe")
	assert.True(health.Operation.Busy, "probe must report busy")
	assert.Equal("checking the search index", health.Operation.Label, "probe label")
	assert.NotNil(health.Operation.StartedAt, "probe start time")

	close(releaseProbe)
	require.Eventually(func() bool {
		_, _, active := srv.currentActivity()
		return !active && srv.ftsIndexComplete.Load()
	}, time.Second, 5*time.Millisecond,
		"activity must clear and the memo flag must set once the probe finishes")
}

// TestHandleCLISearchBackfillProgressUpdatesActivityLabel verifies the
// background FTS backfill mirrors its progress into the health activity
// label, so clients polling /health see live counts rather than a static
// gate label.
func TestHandleCLISearchBackfillProgressUpdatesActivityLabel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := &mockStore{needsFTSBackfill: true}
	engine := &querytest.MockEngine{
		SearchFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
			return nil, nil
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})

	var labelDuringBackfill string
	st.backfillFTSFunc = func(progress func(done, total int64)) (int64, error) {
		progress(2, 4)
		labelDuringBackfill, _, _ = srv.currentActivity()
		return 4, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=hello", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(http.StatusOK, w.Code, "search status: %s", w.Body.String())

	require.Eventually(func() bool { return srv.ftsIndexComplete.Load() },
		time.Second, 5*time.Millisecond, "background backfill must complete")
	assert.Equal("building the search index (2/4 messages)", labelDuringBackfill,
		"activity label must carry backfill progress")
	_, _, active := srv.currentActivity()
	assert.False(active, "activity must be cleared once the backfill finishes")
}

func TestHandleCLIRebuildFTSStreamsProgress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := &mockStore{
		rebuildFTSFunc: func(progress func(done, total int64)) (int64, error) {
			progress(2, 4)
			progress(4, 4)
			return 3, nil
		},
	}
	srv := newCLIHandlerTestServer(st)

	resp := servePOSTTestRequest(srv, "/api/v1/cli/rebuild-fts")

	requireNDJSONResponse(t, resp)
	events := decodeNDJSONEvents[cliRebuildFTSEvent](t, resp.Body)
	require.Len(events, 3, "events")
	assert.Equal("progress", events[0].Type, "first event type")
	assert.Equal(int64(2), events[0].Done, "first done")
	assert.Equal(int64(4), events[0].Total, "first total")
	assert.Equal("progress", events[1].Type, "second event type")
	assert.Equal(int64(4), events[1].Done, "second done")
	assert.Equal(int64(4), events[1].Total, "second total")
	assert.Equal(cliStreamEventTypeComplete, events[2].Type, "final event type")
	assert.Equal(int64(3), events[2].Indexed, "indexed")
}

// TestHandleCLIRebuildFTSInvalidatesCompletenessCache verifies the memoized
// "FTS index complete" flag is cleared while a rebuild runs (so concurrent CLI
// searches re-probe instead of trusting a stale cache) and re-set only after a
// successful rebuild.
func TestHandleCLIRebuildFTSInvalidatesCompletenessCache(t *testing.T) {
	assert := assert.New(t)

	var duringRebuild bool
	st := &mockStore{}
	srv := newCLIHandlerTestServer(st)
	// The FTS index was previously confirmed complete.
	srv.ftsIndexComplete.Store(true)
	// Capture the flag state observed at the start of the rebuild.
	st.rebuildFTSFunc = func(progress func(done, total int64)) (int64, error) {
		duringRebuild = srv.ftsIndexComplete.Load()
		progress(1, 1)
		return 1, nil
	}

	resp := servePOSTTestRequest(srv, "/api/v1/cli/rebuild-fts")
	requireNDJSONResponse(t, resp)
	_ = decodeNDJSONEvents[cliRebuildFTSEvent](t, resp.Body)

	assert.False(duringRebuild, "completeness flag must be cleared before the rebuild runs")
	assert.True(srv.ftsIndexComplete.Load(), "flag must be re-set after a successful rebuild")
}

// TestHandleCLIRebuildFTSFailureLeavesCacheInvalidated verifies a failed
// rebuild leaves the completeness flag cleared, so later CLI searches re-detect
// the (possibly incomplete) index instead of trusting a stale cache.
func TestHandleCLIRebuildFTSFailureLeavesCacheInvalidated(t *testing.T) {
	assert := assert.New(t)

	st := &mockStore{
		rebuildFTSFunc: func(func(done, total int64)) (int64, error) {
			return 0, errors.New("rebuild exploded mid-batch")
		},
	}
	srv := newCLIHandlerTestServer(st)
	srv.ftsIndexComplete.Store(true)

	resp := servePOSTTestRequest(srv, "/api/v1/cli/rebuild-fts")
	requireNDJSONResponse(t, resp)
	_ = decodeNDJSONEvents[cliRebuildFTSEvent](t, resp.Body)

	assert.False(srv.ftsIndexComplete.Load(),
		"a failed rebuild must not leave the completeness flag set")
}

func TestHandleCLIRebuildFTSFlushesProgressThroughMiddleware(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	releaseComplete := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseComplete)
		})
	}
	defer release()

	st := &mockStore{
		rebuildFTSFunc: func(progress func(done, total int64)) (int64, error) {
			progress(2, 4)
			<-releaseComplete
			progress(4, 4)
			return 3, nil
		},
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)

	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		//nolint:bodyclose // The test closes the streaming body after decoding it.
		resp, err := http.Post(httpSrv.URL+"/api/v1/cli/rebuild-fts", "application/json", nil)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	var resp *http.Response
	select {
	case resp = <-respCh:
	case err := <-errCh:
		require.NoError(err, "post rebuild-fts")
	case <-time.After(500 * time.Millisecond):
		require.FailNow("rebuild-fts response did not flush before completion")
	}
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(http.StatusOK, resp.StatusCode, "status")

	decodeCh := make(chan cliRebuildFTSEvent, 1)
	decodeErrCh := make(chan error, 1)
	dec := json.NewDecoder(resp.Body)
	go func() {
		var event cliRebuildFTSEvent
		if err := dec.Decode(&event); err != nil {
			decodeErrCh <- err
			return
		}
		decodeCh <- event
	}()

	var event cliRebuildFTSEvent
	select {
	case event = <-decodeCh:
	case err := <-decodeErrCh:
		require.NoError(err, "decode first progress event")
	case <-time.After(500 * time.Millisecond):
		require.FailNow("rebuild-fts progress event was not flushed before completion")
	}
	assert.Equal("progress", event.Type, "event type")
	assert.Equal(int64(2), event.Done, "done")
	assert.Equal(int64(4), event.Total, "total")

	release()
	for {
		var next cliRebuildFTSEvent
		err := dec.Decode(&next)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(err, "decode remaining event")
		if next.Type == "complete" {
			assert.Equal(int64(3), next.Indexed, "indexed")
			return
		}
	}
	require.FailNow("missing complete event")
}

func TestHandleCLIRebuildFTSBypassesStandardRequestTimeoutWhileQueued(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	gate := NewSerialOperationGate()
	releaseGate, ok := gate.BeginWork()
	require.True(ok, "occupy operation gate")
	defer releaseGate()

	st := &mockStore{
		rebuildFTSFunc: func(progress func(done, total int64)) (int64, error) {
			progress(1, 1)
			return 1, nil
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:         &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:          st,
		Logger:         testLogger(),
		OperationGate:  gate,
		RequestTimeout: 5 * time.Millisecond,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/rebuild-fts", nil)
	resp := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.Router().ServeHTTP(resp, req)
		close(done)
	}()

	time.Sleep(40 * time.Millisecond)
	releaseGate()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		require.FailNow("rebuild-fts request did not complete after gate release")
	}
	assert.Equal(http.StatusOK, resp.Code, "status: %s", resp.Body.String())
}

func TestHandleCLIAccountsReturnsSourceCounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	require.NoError(st.UpdateSourceDisplayName(src.ID, "Alice"), "UpdateSourceDisplayName")
	convID, err := st.EnsureConversation(src.ID, "thread-1", "")
	require.NoError(err, "EnsureConversation")
	_, err = st.UpsertMessage(&store.Message{
		SourceID:        src.ID,
		ConversationID:  convID,
		SourceMessageID: "msg-1",
		MessageType:     "email",
		Subject:         sql.NullString{String: "Hello", Valid: true},
	})
	require.NoError(err, "UpsertMessage")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/accounts", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp struct {
		Accounts []struct {
			ID           int64  `json:"id"`
			Email        string `json:"email"`
			Type         string `json:"type"`
			DisplayName  string `json:"display_name"`
			MessageCount int64  `json:"message_count"`
		} `json:"accounts"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	require.Len(resp.Accounts, 1, "accounts")
	assert.Equal(src.ID, resp.Accounts[0].ID, "ID")
	assert.Equal("alice@example.com", resp.Accounts[0].Email, "Email")
	assert.Equal("gmail", resp.Accounts[0].Type, "Type")
	assert.Equal("Alice", resp.Accounts[0].DisplayName, "DisplayName")
	assert.Equal(int64(1), resp.Accounts[0].MessageCount, "MessageCount")
}

func TestHandleCLIAccountsReturnsOAuthApp(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})

	named, err := st.GetOrCreateSource("gmail", "acme@example.com")
	require.NoError(err, "GetOrCreateSource named")
	require.NoError(
		st.UpdateSourceOAuthApp(named.ID, sql.NullString{String: "acme", Valid: true}),
		"UpdateSourceOAuthApp",
	)
	_, err = st.GetOrCreateSource("gmail", "default@example.com")
	require.NoError(err, "GetOrCreateSource default")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/accounts", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status: %s", w.Body.String())

	var resp struct {
		Accounts []struct {
			Email    string `json:"email"`
			OAuthApp string `json:"oauth_app"`
		} `json:"accounts"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")

	byEmail := map[string]string{}
	for _, a := range resp.Accounts {
		byEmail[a.Email] = a.OAuthApp
	}
	assert.Equal("acme", byEmail["acme@example.com"], "named account oauth_app")
	assert.Empty(byEmail["default@example.com"], "default account oauth_app empty")
}

func TestHandleCLIUpdateAccountDisplayName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/account",
		strings.NewReader(`{"email":"alice@example.com","display_name":"Work"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status: %s", w.Body.String())

	var resp struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal("alice@example.com", resp.Email, "email")
	assert.Equal("Work", resp.DisplayName, "display name")

	updated, err := st.GetSourceByID(src.ID)
	require.NoError(err, "GetSourceByID")
	assert.True(updated.DisplayName.Valid, "stored display name valid")
	assert.Equal("Work", updated.DisplayName.String, "stored display name")
}

func TestHandleCLIUpdateAccountResolvesCurrentDisplayName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Logger: testLogger(),
	})

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	require.NoError(st.UpdateSourceDisplayName(src.ID, "Personal"), "UpdateSourceDisplayName")

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/account",
		strings.NewReader(`{"email":"Personal","display_name":"Work"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status: %s", w.Body.String())

	var resp struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal("alice@example.com", resp.Email, "email")
	assert.Equal("Work", resp.DisplayName, "display name")

	updated, err := st.GetSourceByIdentifier("alice@example.com")
	require.NoError(err, "GetSourceByIdentifier")
	assert.Equal("Work", updated.DisplayName.String, "stored display name")
}

func TestHandleCLIMessageResolvesSourceMessageID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	defer func() { _ = engine.Close() }()
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-1", "")
	require.NoError(err, "EnsureConversation")
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID:        src.ID,
			ConversationID:  convID,
			SourceMessageID: "gmail-42",
			MessageType:     "email",
			Subject:         sql.NullString{String: "Hello", Valid: true},
			SentAt:          sql.NullTime{Time: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true},
		},
		BodyText: sql.NullString{String: "Body text", Valid: true},
	})
	require.NoError(err, "PersistMessage")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/message?id=gmail-42", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp struct {
		ID              int64  `json:"id"`
		SourceMessageID string `json:"source_message_id"`
		Subject         string `json:"subject"`
		BodyText        string `json:"body_text"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal(int64(1), resp.ID, "ID")
	assert.Equal("gmail-42", resp.SourceMessageID, "SourceMessageID")
	assert.Equal("Hello", resp.Subject, "Subject")
	assert.Equal("Body text", resp.BodyText, "BodyText")
}

func TestHandleCLIMessageRawResolvesSourceMessageID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	defer func() { _ = engine.Close() }()
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-raw", "")
	require.NoError(err, "EnsureConversation")
	raw := []byte("From: alice@example.com\r\nSubject: Raw\r\n\r\nBody")
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID:        src.ID,
			ConversationID:  convID,
			SourceMessageID: "gmail-raw",
			MessageType:     "email",
			Subject:         sql.NullString{String: "Raw", Valid: true},
			SentAt:          sql.NullTime{Time: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true},
		},
		RawMIME: raw,
	})
	require.NoError(err, "PersistMessage")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/message/raw?id=gmail-raw", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")
	assert.Equal("message/rfc822", w.Header().Get("Content-Type"), "Content-Type")
	assert.Equal("gmail-raw", w.Header().Get("X-Msgvault-Source-Message-Id"), "SourceMessageID")
	assert.Equal(raw, w.Body.Bytes(), "raw")
}

func TestHandleCLIMessageRawMissingMessageUsesStableErrorCode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	defer func() { _ = engine.Close() }()
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/message/raw?id=missing", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusNotFound, w.Code, "status")
	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal("message_not_found", resp.Error, "error code")
}

func TestHandleCLIMessageRawMissingRawUsesStableErrorCode(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	defer func() { _ = engine.Close() }()
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})

	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-no-raw", "")
	require.NoError(err, "EnsureConversation")
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID:        src.ID,
			ConversationID:  convID,
			SourceMessageID: "gmail-no-raw",
			MessageType:     "email",
			Subject:         sql.NullString{String: "No raw", Valid: true},
			SentAt:          sql.NullTime{Time: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true},
		},
	})
	require.NoError(err, "PersistMessage")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/message/raw?id=gmail-no-raw", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusNotFound, w.Code, "status")
	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal("raw_message_not_found", resp.Error, "error code")
}

func TestHandleCLIAttachmentReturnsContentAddressedBytes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	contentHash := "61ccf192b5bd358738802dc2676d3ceab856f47d26dd29681ac3d335bfd5bbd0"
	data := []byte("attachment bytes")
	attachmentDir := filepath.Join(dataDir, "attachments", contentHash[:2])
	require.NoError(os.MkdirAll(attachmentDir, 0o755), "create attachment dir")
	require.NoError(os.WriteFile(filepath.Join(attachmentDir, contentHash), data, 0o600), "write attachment")

	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{
			Data: config.DataConfig{DataDir: dataDir},
		},
		Logger: testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/attachment?content_hash="+contentHash, nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")
	assert.Equal("application/octet-stream", w.Header().Get("Content-Type"), "Content-Type")
	assert.Equal(contentHash, w.Header().Get("X-Msgvault-Content-Hash"), "ContentHash")
	assert.Equal(data, w.Body.Bytes(), "data")
}

// buildTestPack seals content into a single-blob pack under
// attachmentsDir/packs/ and returns the resulting store index entry. Not a
// Test-prefixed function, so testify-helper-check does not require a local
// assert/require helper here — and critically, keeping "require" unshadowed
// in the caller lets each t.Run subtest below declare its own
// require.New(t)/assert.New(t) bound to the subtest's own *testing.T.
func buildTestPack(t *testing.T, attachmentsDir string, content []byte) store.PackIndexEntry {
	t.Helper()
	staging := t.TempDir()
	w, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(t, err, "NewWriter")
	_, err = w.Append(content)
	require.NoError(t, err, "Append")
	packID := w.ID()
	final := filepath.Join(attachmentsDir, "packs", packID[:2], packID+packstore.PackExt)
	require.NoError(t, os.MkdirAll(filepath.Dir(final), 0o700), "create pack dir")
	sealed, err := w.Seal(final)
	require.NoError(t, err, "Seal")
	require.Len(t, sealed, 1, "sealed entries")

	return store.PackIndexEntry{
		BlobHash:  sealed[0].ID.String(),
		PackID:    packID,
		Offset:    int64(sealed[0].Offset),
		StoredLen: int64(sealed[0].StoredLen),
		RawLen:    int64(sealed[0].RawLen),
		Flags:     uint8(sealed[0].Flags),
		CRC32C:    sealed[0].CRC32C,
	}
}

// TestCLIAttachmentServesPackedBlob covers the daemon attachment endpoint's
// blobstore-backed path: a packed blob is served from a sealed pack, an
// unknown hash 404s, and a loose file is still served through the same
// BlobStore-configured server (the nil-BlobStore path is exercised
// separately by TestHandleCLIAttachmentReturnsContentAddressedBytes).
func TestCLIAttachmentServesPackedBlob(t *testing.T) {
	must := require.New(t)
	dataDir := t.TempDir()
	attachmentsDir := filepath.Join(dataDir, "attachments")

	content := []byte("packed attachment bytes")
	entry := buildTestPack(t, attachmentsDir, content)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	must.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "packed-thread", "Packed Thread")
	must.NoError(err, "EnsureConversation")
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID: convID, SourceID: src.ID,
		SourceMessageID: "packed-message", MessageType: "email",
	})
	must.NoError(err, "UpsertMessage")
	must.NoError(st.UpsertAttachment(msgID, "packed.bin", "application/octet-stream",
		entry.BlobHash[:2]+"/"+entry.BlobHash, entry.BlobHash, len(content)), "UpsertAttachment packed")
	must.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID:      entry.PackID,
		EntryCount:  1,
		StoredBytes: entry.StoredLen,
		CreatedAt:   time.Now(),
	}, []store.PackIndexEntry{entry}), "RecordPackedBlobs")

	bs, err := attachmentstore.New(store.NewPackCatalog(st), attachmentsDir)
	require.NoError(t, err)
	// Close cached pack readers before t.TempDir cleanup; Windows cannot
	// delete the .mvpack file while a reader holds it open.
	t.Cleanup(func() { assert.NoError(t, bs.Close(), "close blob store") })

	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{
			Data: config.DataConfig{DataDir: dataDir},
		},
		Logger:    testLogger(),
		BlobStore: bs,
		Engine:    query.NewEngine(st.DB(), st.IsPostgreSQL()),
	})

	t.Run("packed blob returns 200 with body", func(t *testing.T) {
		assert := assert.New(t)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/attachment?content_hash="+entry.BlobHash, nil)
		w := httptest.NewRecorder()

		srv.Router().ServeHTTP(w, req)

		assert.Equal(http.StatusOK, w.Code, "status")
		assert.Equal("application/octet-stream", w.Header().Get("Content-Type"), "Content-Type")
		assert.Equal(entry.BlobHash, w.Header().Get("X-Msgvault-Content-Hash"), "ContentHash")
		assert.Equal(content, w.Body.Bytes(), "data")
	})

	t.Run("public content endpoint serves packed blob", func(t *testing.T) {
		assert := assert.New(t)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/attachments/"+entry.BlobHash+"/content", nil)
		w := httptest.NewRecorder()

		srv.Router().ServeHTTP(w, req)

		assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
		assert.Equal("application/octet-stream", w.Header().Get("Content-Type"), "Content-Type")
		assert.Equal(`attachment; filename="packed.bin"`, w.Header().Get("Content-Disposition"), "Content-Disposition")
		assert.Equal(content, w.Body.Bytes(), "data")
	})

	t.Run("public content endpoint serves legacy namespaced loose blob", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		hash := "5bc8e75b9fa9867b99fe7498fcb611a46ab49326ff94a96b2a6f68a723f46d91"
		legacyContent := []byte("legacy namespaced attachment")
		storagePath := filepath.Join("synctech-sms", hash[:2], hash)
		require.NoError(st.UpsertAttachment(msgID, "legacy.bin", "application/octet-stream",
			storagePath, hash, len(legacyContent)), "UpsertAttachment legacy")
		require.NoError(os.MkdirAll(filepath.Join(attachmentsDir, filepath.Dir(storagePath)), 0o700), "create legacy dir")
		require.NoError(os.WriteFile(filepath.Join(attachmentsDir, storagePath), legacyContent, 0o600), "write legacy attachment")

		req := httptest.NewRequest(http.MethodGet, "/api/v1/attachments/"+hash+"/content", nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)

		assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
		assert.Equal(legacyContent, w.Body.Bytes(), "data")
	})

	t.Run("public content endpoint tries every duplicate hash path", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		hash := "80c92207dd517f7edb6479ca8694460ed21ddf969f52e17e2ba7808f706cce74"
		availableContent := []byte("available duplicate attachment")
		availablePath := filepath.Join("synctech-sms", hash[:2], hash)

		require.NoError(st.UpsertAttachment(msgID, "missing.bin", "application/x-missing",
			filepath.Join("missing", hash), hash, len(availableContent)), "UpsertAttachment missing duplicate")
		availableMsgID, err := st.UpsertMessage(&store.Message{
			ConversationID: convID, SourceID: src.ID,
			SourceMessageID: "available-duplicate-message", MessageType: "email",
		})
		require.NoError(err, "UpsertMessage available duplicate")
		require.NoError(st.UpsertAttachment(availableMsgID, "available.bin", "text/plain",
			availablePath, hash, len(availableContent)), "UpsertAttachment available duplicate")
		require.NoError(os.MkdirAll(filepath.Join(attachmentsDir, filepath.Dir(availablePath)), 0o700), "create available duplicate dir")
		require.NoError(os.WriteFile(filepath.Join(attachmentsDir, availablePath), availableContent, 0o600), "write available duplicate")

		req := httptest.NewRequest(http.MethodGet, "/api/v1/attachments/"+hash+"/content", nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)

		assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
		assert.Equal("text/plain", w.Header().Get("Content-Type"), "Content-Type")
		assert.Equal(`attachment; filename="available.bin"`, w.Header().Get("Content-Disposition"), "Content-Disposition")
		assert.Equal(availableContent, w.Body.Bytes(), "data")
	})

	t.Run("unknown hash returns 404", func(t *testing.T) {
		assert := assert.New(t)
		unknownHash := "bf0803740e367e2f5e45f0813f62b9c4c09a44e8a649a39970295a9322859048"
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/attachment?content_hash="+unknownHash, nil)
		w := httptest.NewRecorder()

		srv.Router().ServeHTTP(w, req)

		assert.Equal(http.StatusNotFound, w.Code, "status")
	})

	t.Run("loose file fallback still served", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		looseHash := "220a1f4bfb5bd7d3fc13a5b9475afc62f88b94bbd7177ea4581e8049288a6bd2"
		looseContent := []byte("loose fallback bytes")
		require.NoError(st.UpsertAttachment(msgID, "loose.bin", "application/octet-stream",
			looseHash[:2]+"/"+looseHash, looseHash, len(looseContent)), "UpsertAttachment loose")
		looseDir := filepath.Join(attachmentsDir, looseHash[:2])
		require.NoError(os.MkdirAll(looseDir, 0o755), "create loose dir")
		require.NoError(os.WriteFile(filepath.Join(looseDir, looseHash), looseContent, 0o600), "write loose attachment")

		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/attachment?content_hash="+looseHash, nil)
		w := httptest.NewRecorder()

		srv.Router().ServeHTTP(w, req)

		assert.Equal(http.StatusOK, w.Code, "status")
		assert.Equal("application/octet-stream", w.Header().Get("Content-Type"), "Content-Type")
		assert.Equal(looseContent, w.Body.Bytes(), "data")
	})
}

func TestHandleListMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	messages, ok := resp["messages"].([]any)
	require.True(ok, "expected messages array in response")
	assert.NotEmpty(messages, "expected at least 1 message")

	// Check first message structure
	msg, ok := messages[0].(map[string]any)
	require.True(ok, "message is map[string]any")
	assert.Equal("Test Subject", msg["subject"], "subject")

	// Verify RFC3339 time format
	sentAt, ok := msg["sent_at"].(string)
	require.True(ok, "sent_at is string")
	_, err := time.Parse(time.RFC3339, sentAt)
	assert.NoError(err, "sent_at %q is not RFC3339", sentAt)
}

func TestHandleListMessagesPagination(t *testing.T) {
	assert := assert.New(t)
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages?page=1&page_size=10", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.InDelta(float64(1), resp["page"], 1e-9, "page")
	assert.InDelta(float64(10), resp["page_size"], 1e-9, "page_size")
}

// TestPageSizeClamping verifies that /messages and /search clamp an
// over-max page_size to the max (100) rather than collapsing it to the
// default (20), while absent/too-small values fall back to the default and
// non-numeric values are rejected with 400.
func TestPageSizeClamping(t *testing.T) {
	cases := []struct {
		name         string
		query        string
		wantStatus   int
		wantPageSize int
	}{
		{"over_max_clamps", "page_size=101", http.StatusOK, 100},
		{"far_over_max_clamps", "page_size=100000", http.StatusOK, 100},
		{"at_max_unchanged", "page_size=100", http.StatusOK, 100},
		{"in_range_unchanged", "page_size=5", http.StatusOK, 5},
		{"absent_default", "", http.StatusOK, 20},
		{"zero_default", "page_size=0", http.StatusOK, 20},
		{"negative_default", "page_size=-1", http.StatusOK, 20},
		{"garbage_rejected", "page_size=abc", http.StatusBadRequest, 0},
	}

	endpoints := []struct {
		name string
		path string
	}{
		{"messages", "/api/v1/messages"},
		{"search", "/api/v1/search?q=test"},
	}

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					srv, _ := newTestServerWithMockStore(t)
					sep := "?"
					if strings.Contains(ep.path, "?") {
						sep = "&"
					}
					reqURL := ep.path
					if tc.query != "" {
						reqURL += sep + tc.query
					}
					req := httptest.NewRequest(http.MethodGet, reqURL, nil)
					w := httptest.NewRecorder()
					srv.Router().ServeHTTP(w, req)

					assert.Equal(t, tc.wantStatus, w.Code, "status")
					if tc.wantStatus != http.StatusOK {
						return
					}
					var resp map[string]any
					require.NoError(t, json.NewDecoder(w.Body).Decode(&resp),
						"decode response")
					assert.InDelta(t, float64(tc.wantPageSize),
						resp["page_size"], 1e-9, "page_size")
				})
			}
		})
	}
}

func TestHandleSourceStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	sched := newMockScheduler()
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())

	gmail, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource gmail")
	require.NoError(st.UpdateSourceDisplayName(gmail.ID, "Alice"), "UpdateSourceDisplayName")
	require.NoError(st.UpdateSourceSyncCursor(gmail.ID, "history-1"), "UpdateSourceSyncCursor")

	completedID, err := st.StartSync(gmail.ID, "full")
	require.NoError(err, "StartSync completed")
	require.NoError(st.UpdateSyncCheckpoint(completedID, &store.Checkpoint{
		PageToken:         "page-1",
		MessagesProcessed: 10,
		MessagesAdded:     8,
		MessagesUpdated:   2,
	}), "UpdateSyncCheckpoint")

	require.NoError(st.RecordSyncRunItem(store.SyncRunItem{
		SyncRunID:       completedID,
		SourceMessageID: "gmail-missing",
		Phase:           "fetch",
		Status:          store.SyncRunItemStatusSkipped,
		ErrorKind:       "gmail_not_found",
		ErrorMessage:    "not found: /messages/gmail-missing",
	}), "RecordSyncRunItem skipped")

	require.NoError(st.RecordSyncRunItem(store.SyncRunItem{
		SyncRunID:       completedID,
		SourceMessageID: "gmail-error",
		Phase:           "ingest",
		Status:          store.SyncRunItemStatusError,
		ErrorKind:       "ingest_error",
		ErrorMessage:    "parse MIME: malformed header",
	}), "RecordSyncRunItem error")

	require.NoError(st.CompleteSync(completedID, "history-2"), "CompleteSync")

	runningID, err := st.StartSync(gmail.ID, "incremental")
	require.NoError(err, "StartSync running")

	_, err = st.GetOrCreateSource("imap", "imaps://mail.example.com/alice")
	require.NoError(err, "GetOrCreateSource imap")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sources/status?source_type=gmail", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp SourceStatusResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	require.Len(resp.Sources, 1, "sources")

	got := resp.Sources[0]
	assert.Equal(gmail.ID, got.ID, "ID")
	assert.Equal("gmail", got.SourceType, "SourceType")
	assert.Equal("alice@example.com", got.Identifier, "Identifier")
	require.NotNil(got.DisplayName, "DisplayName")
	assert.Equal("Alice", *got.DisplayName, "DisplayName")
	require.NotNil(got.LastSyncAt, "LastSyncAt")
	assert.NotEmpty(*got.LastSyncAt, "LastSyncAt")
	assert.NotEmpty(got.UpdatedAt, "UpdatedAt")

	require.NotNil(got.ActiveSync, "ActiveSync")
	assert.Equal(runningID, got.ActiveSync.ID, "ActiveSync.ID")
	assert.Equal(store.SyncStatusRunning, got.ActiveSync.Status, "ActiveSync.Status")

	require.NotNil(got.LatestSync, "LatestSync")
	assert.Equal(runningID, got.LatestSync.ID, "LatestSync.ID")

	require.NotNil(got.LastSuccessfulSync, "LastSuccessfulSync")
	assert.Equal(completedID, got.LastSuccessfulSync.ID, "LastSuccessfulSync.ID")
	assert.Equal(store.SyncStatusCompleted, got.LastSuccessfulSync.Status, "LastSuccessfulSync.Status")
	assert.Equal(int64(10), got.LastSuccessfulSync.MessagesProcessed, "LastSuccessfulSync.MessagesProcessed")
	assert.Equal(int64(1), got.LastSuccessfulSync.SkippedCount, "LastSuccessfulSync.SkippedCount")
	require.Len(got.LastSuccessfulSync.ItemErrors, 1, "LastSuccessfulSync.ItemErrors")
	assert.Equal("gmail-error", got.LastSuccessfulSync.ItemErrors[0].SourceMessageID, "LastSuccessfulSync.ItemErrors[0].SourceMessageID")
	assert.Equal("ingest", got.LastSuccessfulSync.ItemErrors[0].Phase, "LastSuccessfulSync.ItemErrors[0].Phase")
	assert.Equal("ingest_error", got.LastSuccessfulSync.ItemErrors[0].ErrorKind, "LastSuccessfulSync.ItemErrors[0].ErrorKind")
	assert.Equal("parse MIME: malformed header", got.LastSuccessfulSync.ItemErrors[0].ErrorMessage, "LastSuccessfulSync.ItemErrors[0].ErrorMessage")
	assert.NotEmpty(got.LastSuccessfulSync.ItemErrors[0].CreatedAt, "LastSuccessfulSync.ItemErrors[0].CreatedAt")
	require.NotNil(got.LastSuccessfulSync.CursorAfter, "LastSuccessfulSync.CursorAfter")
	assert.Equal("history-2", *got.LastSuccessfulSync.CursorAfter, "LastSuccessfulSync.CursorAfter")
}

func TestHandleSourceStatusNoSyncRuns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	source, err := st.GetOrCreateSource("gmail", "empty@example.com")
	require.NoError(err, "GetOrCreateSource")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sources/status", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp SourceStatusResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	require.Len(resp.Sources, 1, "sources")
	assert.Equal(source.ID, resp.Sources[0].ID, "ID")
	assert.Nil(resp.Sources[0].ActiveSync, "ActiveSync")
	assert.Nil(resp.Sources[0].LatestSync, "LatestSync")
	assert.Nil(resp.Sources[0].LastSuccessfulSync, "LastSuccessfulSync")
}

func TestHandleGetMessage(t *testing.T) {
	assert := assert.New(t)
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/1", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp MessageDetail
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal(int64(1), resp.ID, "id")
	assert.Equal("Test Subject", resp.Subject, "subject")
	assert.Equal("This is the full message body text.", resp.Body, "body")
}

func TestHandleGetMessageNotFound(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/99999", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "status")
}

func TestHandleGetMessageInvalidID(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "status")
}

func TestHandleGetMessage_EngineBodyHTML(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	engine := &querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{
			42: {
				ID:              42,
				SourceMessageID: "source-42",
				Subject:         "HTML Email",
				From:            []query.Address{{Email: "sender@example.com", Name: "Sender"}},
				To:              []query.Address{{Email: "rcpt@example.com"}},
				SentAt:          time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
				Labels:          []string{"INBOX"},
				BodyText:        "plain fallback",
				BodyHTML:        "<p>Hello</p>",
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/42", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode")
	assert.Equal("plain fallback", resp["body"], "body")
	assert.Equal("<p>Hello</p>", resp["body_html"], "body_html")
	assert.Equal("source-42", resp["source_message_id"], "source_message_id")
	assert.Equal("HTML Email", resp["subject"], "subject")
	assert.Equal("Sender <sender@example.com>", resp["from"], "from")
	assert.NotContains(resp, "deleted_at", "deleted_at should be omitted for live message")
}

// TestHandleGetMessage_EngineDeletedAt verifies the engine path surfaces
// deleted_at in the response when the underlying message has a
// deleted_from_source_at timestamp.
func TestHandleGetMessage_EngineDeletedAt(t *testing.T) {
	deletedAt := time.Date(2024, 7, 1, 9, 30, 0, 0, time.UTC)
	engine := &querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{
			42: {
				ID:        42,
				Subject:   "Deleted",
				From:      []query.Address{{Email: "sender@example.com"}},
				SentAt:    time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
				DeletedAt: &deletedAt,
				BodyText:  "hello",
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/42", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode")
	want := deletedAt.Format(time.RFC3339)
	assert.Equal(t, want, resp["deleted_at"], "deleted_at")
}

func TestHandleSearchMissingQuery(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "status")
}

func TestHandleSearch(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=Test", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "status")

	var resp SearchResult
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal(t, "Test", resp.Query, "query")
}

func TestHandleSearchInvalidOperatorValueReturns400(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantValue string
	}{
		{"bad_date", "test before:2025-13-45", "2025-13-45"},
		{"bad_size", "larger:5X", "5X"},
		{"bad_age", "older_than:99q", "99q"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, st := newTestServerWithMockStore(t)

			req := httptest.NewRequest(http.MethodGet,
				"/api/v1/search?q="+url.QueryEscape(tc.query), nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			require.Equal(http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())
			assert.Equal(int32(0), st.searchMessagesCalls.Load(), "SearchMessages must not run")
			assert.Equal(int32(0), st.searchMessagesQueryCalls.Load(), "SearchMessagesQuery must not run")

			var resp ErrorResponse
			require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode error body")
			assert.Equal("invalid_query", resp.Error, "error code")
			assert.Contains(resp.Message, tc.wantValue, "message names bad value")
		})
	}
}

func TestHandleSearchPlainTextAccountScopeUsesStructuredSearch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newTestServerWithMockStore(t)
	st.sourcesByLookup = map[string][]*store.Source{
		"alice@example.com": {
			{ID: 77, SourceType: "gmail", Identifier: "alice@example.com"},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=lunch&account=alice@example.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal(int32(0), st.searchMessagesCalls.Load(), "SearchMessages calls")
	assert.Equal(int32(1), st.searchMessagesQueryCalls.Load(), "SearchMessagesQuery calls")
	require.NotNil(st.searchMessagesQueryLast, "structured query")
	assert.Equal([]int64{77}, st.searchMessagesQueryLast.AccountIDs, "AccountIDs")
}

func TestHandleSearchAccountLookupErrorReturnsInternal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newTestServerWithMockStore(t)
	st.sourcesByLookupErr = errors.New("source index unavailable")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=lunch&account=alice@example.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusInternalServerError, w.Code, "status: %s", w.Body.String())

	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode error response")
	assert.Equal("internal_error", resp.Error, "error code")
	assert.Equal("Failed to resolve CLI scope", resp.Message, "error message")
}

func TestHandleTriggerSync(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Accounts: []config.AccountSchedule{
			{Email: "test@gmail.com", Schedule: "0 2 * * *", Enabled: true},
		},
	}

	sched := newMockScheduler()
	sched.scheduled["test@gmail.com"] = true

	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/test@gmail.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code, "status (body: %s)", w.Body.String())
}

func TestHandleTriggerSyncNotFound(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}

	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/unknown@gmail.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "status")
}

func TestHandleTriggerSyncConflict(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}

	sched := newMockScheduler()
	sched.scheduled["test@gmail.com"] = true
	sched.triggerFn = func(email string) error {
		return errors.New("sync already running for test@gmail.com")
	}

	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/test@gmail.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code, "status")
}

func TestErrorResponseShape(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	// Test with invalid ID to get a 400 error
	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode error response")

	assert.NotEmpty(t, resp.Error, "expected error code in response")
	assert.NotEmpty(t, resp.Message, "expected error message in response")
}

func TestMessageSummaryNilSlices(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := &mockStore{
		messages: []APIMessage{
			{
				ID:      1,
				Subject: "No recipients",
				To:      nil,
				Labels:  nil,
			},
		},
		total: 1,
	}

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, store, sched, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	// Verify nil slices become empty arrays, not null
	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	messages, ok := resp["messages"].([]any)
	require.True(ok, "messages is []any")
	msg, ok := messages[0].(map[string]any)
	require.True(ok, "message is map[string]any")

	// "to" should be an empty array, not null
	to, ok := msg["to"].([]any)
	require.True(ok, "expected 'to' to be an array, got %T", msg["to"])
	assert.Empty(to, "expected empty 'to' array")

	labels, ok := msg["labels"].([]any)
	require.True(ok, "expected 'labels' to be an array, got %T", msg["labels"])
	assert.Empty(labels, "expected empty 'labels' array")
}

func TestMessageSummaryCcBccInResponse(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, ms := newTestServerWithMockStore(t)
	ms.messages[0].Cc = []string{"cc1@example.com", "cc2@example.com"}
	ms.messages[0].Bcc = []string{"bcc@example.com"}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status")

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")

	msgs, ok := resp["messages"].([]any)
	require.True(ok, "messages is []any")
	msg, ok := msgs[0].(map[string]any)
	require.True(ok, "message is map[string]any")

	ccRaw, ok := msg["cc"].([]any)
	require.True(ok, "expected 'cc' array, got %T", msg["cc"])
	var gotCc []string
	for _, v := range ccRaw {
		s, ok := v.(string)
		require.True(ok, "cc element is string, got %T", v)
		gotCc = append(gotCc, s)
	}
	slices.Sort(gotCc)
	wantCc := []string{"cc1@example.com", "cc2@example.com"}
	assert.Equal(wantCc, gotCc, "cc")

	bcc, ok := msg["bcc"].([]any)
	require.True(ok, "expected 'bcc' array, got %T", msg["bcc"])
	require.Len(bcc, 1, "bcc")
	assert.Equal("bcc@example.com", bcc[0], "bcc[0]")
}

func TestMessageSummaryCcBccOmittedWhenEmpty(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	// Parse raw JSON to check field presence
	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal(w.Body.Bytes(), &raw), "decode")

	var messages []json.RawMessage
	require.NoError(json.Unmarshal(raw["messages"], &messages), "decode messages")

	var msg map[string]json.RawMessage
	require.NoError(json.Unmarshal(messages[0], &msg), "decode message")

	assert.NotContains(msg, "cc", "expected 'cc' to be omitted from JSON when empty")
	assert.NotContains(msg, "bcc", "expected 'bcc' to be omitted from JSON when empty")
}

func TestGetMessageCcBccInResponse(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, ms := newTestServerWithMockStore(t)
	ms.messages[0].Cc = []string{"cc@example.com"}
	ms.messages[0].Bcc = []string{"bcc@example.com"}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/1", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status")

	var resp MessageDetail
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")

	require.Len(resp.Cc, 1, "cc")
	assert.Equal("cc@example.com", resp.Cc[0], "cc[0]")
	require.Len(resp.Bcc, 1, "bcc")
	assert.Equal("bcc@example.com", resp.Bcc[0], "bcc[0]")
}

func TestGetMessageIncludesAttachmentID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, ms := newTestServerWithMockStore(t)
	ms.messages[0].HasAttachments = true
	ms.messages[0].Attachments = []APIAttachment{{
		ID:          77,
		Filename:    "report.pdf",
		MimeType:    "application/pdf",
		Size:        1234,
		ContentHash: "hash-77",
		URL:         "/api/v1/attachments/77",
	}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/1", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status")

	var resp MessageDetail
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")

	require.Len(resp.Attachments, 1, "attachments")
	assert.Equal(int64(77), resp.Attachments[0].ID, "attachment id")
}

func TestHandleUploadToken(t *testing.T) {
	// Create temp directory for tokens
	tmpDir := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Data:   config.DataConfig{DataDir: tmpDir},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	tokenJSON := `{
		"access_token": "ya29.test",
		"token_type": "Bearer",
		"refresh_token": "1//test-refresh-token",
		"expiry": "2024-12-31T23:59:59Z"
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token/test@gmail.com", strings.NewReader(tokenJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code, "status (body: %s)", w.Body.String())

	// Verify token file was created
	tokenPath := filepath.Join(tmpDir, "tokens", "test@gmail.com.json")
	_, statErr := os.Stat(tokenPath)
	assert.False(t, os.IsNotExist(statErr), "token file was not created at %s", tokenPath)
}

func TestHandleUploadToken_PreservesClientID(t *testing.T) {
	require := require.New(t)
	tmpDir := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Data:   config.DataConfig{DataDir: tmpDir},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	tokenJSON := `{
		"access_token": "ya29.test",
		"token_type": "Bearer",
		"refresh_token": "1//test-refresh-token",
		"client_id": "myapp.apps.googleusercontent.com"
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token/test@gmail.com", strings.NewReader(tokenJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusCreated, w.Code, "status (body: %s)", w.Body.String())

	// Read back the saved token and verify client_id was preserved
	tokenPath := filepath.Join(tmpDir, "tokens", "test@gmail.com.json")
	data, err := os.ReadFile(tokenPath)
	require.NoError(err, "read token file")

	var saved struct {
		ClientID string `json:"client_id"`
	}
	require.NoError(json.Unmarshal(data, &saved), "unmarshal saved token")
	assert.Equal(t, "myapp.apps.googleusercontent.com", saved.ClientID, "client_id")
}

func TestHandleUploadTokenInvalidJSON(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Data:   config.DataConfig{DataDir: tmpDir},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token/test@gmail.com", strings.NewReader("not valid json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusBadRequest, w.Code, "status")

	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode error response")
	assert.Equal("invalid_json", resp.Error, "error")
}

func TestHandleUploadTokenMissingRefreshToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Data:   config.DataConfig{DataDir: tmpDir},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	// Token without refresh_token
	tokenJSON := `{
		"access_token": "ya29.test",
		"token_type": "Bearer"
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token/test@gmail.com", strings.NewReader(tokenJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusBadRequest, w.Code, "status")

	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode error response")
	assert.Equal("invalid_token", resp.Error, "error")
}

func TestHandleUploadTokenInvalidEmail(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	tokenJSON := `{"refresh_token": "test"}`

	tests := []struct {
		name  string
		email string
	}{
		{"no at sign", "testgmail.com"},
		{"no domain", "test@"},
		{"no dot", "test@gmailcom"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token/"+tc.email, strings.NewReader(tokenJSON))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			srv.Router().ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code, "status for email %q", tc.email)
		})
	}
}

func TestHandleUploadTokenMissingEmail(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	// Request without email in path - should 404 since route doesn't match
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token/", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	// Chi router will 404 on missing path parameter
	assert.Contains(t, []int{http.StatusNotFound, http.StatusBadRequest}, w.Code, "status = %d, want 404 or 400", w.Code)
}

func TestHandleAddAccount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()

	cfg := &config.Config{
		Server:  config.ServerConfig{APIPort: 8080},
		HomeDir: tmpDir,
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	body := `{"email": "new@gmail.com", "schedule": "0 3 * * *"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusCreated, w.Code, "status (body: %s)", w.Body.String())

	// Verify account was added to config
	require.Len(cfg.Accounts, 1, "expected 1 account")
	assert.Equal("new@gmail.com", cfg.Accounts[0].Email, "email")

	// Verify scheduler was notified
	require.Len(sched.addedAccts, 1, "scheduler.AddAccount not called, addedAccts = %v", sched.addedAccts)
	assert.Equal("new@gmail.com", sched.addedAccts[0], "scheduler.AddAccount account")
}

func TestHandleAddAccountDuplicate(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Accounts: []config.AccountSchedule{
			{Email: "existing@gmail.com", Schedule: "0 2 * * *", Enabled: true},
		},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	body := `{"email": "existing@gmail.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "status")

	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.Equal(t, "exists", resp["status"], "status")
}

func TestHandleAddAccountInvalidCron(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	body := `{"email": "new@gmail.com", "schedule": "not a cron"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())

	var resp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.Equal(t, "invalid_schedule", resp.Error, "error")
}

func TestHandleAddAccountInvalidEmail(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	tests := []struct {
		name string
		body string
		code int
	}{
		{"empty email", `{"email": ""}`, http.StatusBadRequest},
		{"no at sign", `{"email": "nope"}`, http.StatusBadRequest},
		{"no dot", `{"email": "nope@nope"}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			assert.Equal(t, tt.code, w.Code, "status")
		})
	}
}

func TestHandleAddAccountSaveFailure(t *testing.T) {
	// Point HomeDir to a file (not a directory) so Save() fails
	tmpFile := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(tmpFile, []byte("x"), 0600), "create blocker file")

	cfg := &config.Config{
		Server:  config.ServerConfig{APIPort: 8080},
		HomeDir: tmpFile, // Save() will fail: can't mkdir inside a file
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	body := `{"email": "fail@gmail.com", "schedule": "0 2 * * *"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code, "status")

	// In-memory state should be rolled back
	assert.Empty(t, cfg.Accounts, "cfg.Accounts has %d entries, want 0 (rollback failed)", len(cfg.Accounts))
}

func TestSanitizeTokenPath(t *testing.T) {
	tokensDir := "/data/tokens"

	tests := []struct {
		name  string
		email string
	}{
		{"normal email", "user@gmail.com"},
		{"email with plus", "user+tag@gmail.com"},
		{"email with dots", "first.last@gmail.com"},
		{"path traversal attempt", "../../../etc/passwd"},
		{"slash in email", "user/evil@gmail.com"},
		{"backslash in email", "user\\evil@gmail.com"},
		{"null byte", "user\x00evil@gmail.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := sanitizeTokenPath(tokensDir, tc.email)

			// Result must be within tokensDir (path traversal prevention)
			cleanResult := filepath.Clean(result)
			cleanTokensDir := filepath.Clean(tokensDir)
			assert.True(t, strings.HasPrefix(cleanResult, cleanTokensDir+string(os.PathSeparator)),
				"path %q escapes tokensDir %q", result, tokensDir)

			// Result must end with .json
			assert.True(t, strings.HasSuffix(result, ".json"), "path %q doesn't end with .json", result)

			// Result must not contain path separators in the filename
			base := filepath.Base(result)
			assert.False(t, strings.ContainsAny(base, "/\\"), "filename %q contains path separators", base)
		})
	}
}

// newTestServerWithEngine creates a test server with both mock store and mock engine.
func newTestServerWithEngine(t *testing.T, engine query.Engine) *Server {
	t.Helper()

	store := &mockStore{
		stats: &StoreStats{
			MessageCount:    10,
			ThreadCount:     5,
			SourceCount:     1,
			LabelCount:      3,
			AttachmentCount: 2,
			DatabaseSize:    1024,
		},
		messages: []APIMessage{
			{
				ID:             1,
				Subject:        "Test Subject",
				From:           "sender@example.com",
				To:             []string{"recipient@example.com"},
				SentAt:         time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				Snippet:        "This is a test message snippet",
				Labels:         []string{"INBOX"},
				HasAttachments: false,
				SizeEstimate:   1024,
				Body:           "This is the full message body text.",
			},
		},
		total: 1,
	}

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()

	srv := NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     store,
		Engine:    engine,
		Scheduler: sched,
		Logger:    testLogger(),
	})
	return srv
}

func TestHandleAggregates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	engine := &querytest.MockEngine{
		AggregateRows: []query.AggregateRow{
			{Key: "alice@example.com", Count: 100, TotalSize: 50000, AttachmentSize: 10000, AttachmentCount: 5},
			{Key: "bob@example.com", Count: 50, TotalSize: 25000, AttachmentSize: 5000, AttachmentCount: 2},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/aggregates?view_type=senders", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp AggregateResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal("senders", resp.ViewType, "view_type")
	require.Len(resp.Rows, 2, "rows count")
	assert.Equal("alice@example.com", resp.Rows[0].Key, "first row key")
}

func TestHandleAggregatesNoEngine(t *testing.T) {
	// Server without engine
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     nil,
		Engine:    nil,
		Scheduler: sched,
		Logger:    testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/aggregates?view_type=senders", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "status")
}

func TestHandleAggregatesInvalidViewType(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/aggregates?view_type=invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "status")
}

func TestHandleSubAggregates(t *testing.T) {
	assert := assert.New(t)
	engine := &querytest.MockEngine{
		AggregateRows: []query.AggregateRow{
			{Key: "INBOX", Count: 80, TotalSize: 40000},
			{Key: "SENT", Count: 20, TotalSize: 10000},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/aggregates/sub?view_type=labels&sender=alice@example.com", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp AggregateResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal("labels", resp.ViewType, "view_type")
	assert.Len(resp.Rows, 2, "rows count")
}

func TestHandleFilteredMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var gotFilter query.MessageFilter
	engine := &querytest.MockEngine{
		ListMessagesFunc: func(_ context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
			gotFilter = filter
			return []query.MessageSummary{{
				ID:             1,
				Subject:        "Test Email",
				MessageType:    "sms",
				FromEmail:      "alice@example.com",
				SentAt:         time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				Labels:         []string{"INBOX"},
				HasAttachments: false,
				SizeEstimate:   1024,
			}}, nil
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/filter?sender=alice@example.com&message_type=sms&limit=100", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	messages, ok := resp["messages"].([]any)
	require.True(ok, "expected messages array in response")
	assert.Len(messages, 1, "messages count")
	require.Equal("sms", gotFilter.MessageType, "message_type filter")
	msg, ok := messages[0].(map[string]any)
	require.True(ok, "message row = %#v, want object", messages[0])
	require.Equal("sms", msg["message_type"], "response message_type")
}

func TestHandleGmailIDsByFilterUsesQueryEngine(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var gotFilter query.MessageFilter
	engine := &querytest.MockEngine{
		GetGmailIDsByFilterFunc: func(_ context.Context, filter query.MessageFilter) ([]string, error) {
			gotFilter = filter
			return []string{"gm-1", "gm-2"}, nil
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/gmail-ids?sender=alice@example.com&message_type=email", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal("alice@example.com", gotFilter.Sender, "sender filter")
	assert.Equal("email", gotFilter.MessageType, "message type filter")

	var resp struct {
		GmailIDs []string `json:"gmail_ids"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal([]string{"gm-1", "gm-2"}, resp.GmailIDs, "gmail_ids")
}

func TestHandleGetAttachmentUsesQueryEngine(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	engine := &querytest.MockEngine{
		Attachments: map[int64]*query.AttachmentInfo{
			42: {
				ID:          42,
				Filename:    "report.pdf",
				MimeType:    "application/pdf",
				Size:        12345,
				ContentHash: "hash-42",
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/attachments/42", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp AttachmentInfo
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal(int64(42), resp.ID, "id")
	assert.Equal("report.pdf", resp.Filename, "filename")
	assert.Equal("application/pdf", resp.MimeType, "mime_type")
	assert.Equal(int64(12345), resp.Size, "size")
	assert.Equal("hash-42", resp.ContentHash, "content_hash")
}

func TestHandleSearchByDomainsUsesQueryEngine(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var gotDomains []string
	var gotAfter, gotBefore *time.Time
	var gotLimit, gotOffset int
	engine := &querytest.MockEngine{
		SearchByDomainsFunc: func(_ context.Context, domains []string, after, before *time.Time, limit, offset int) ([]query.MessageSummary, error) {
			gotDomains = domains
			gotAfter = after
			gotBefore = before
			gotLimit = limit
			gotOffset = offset
			return []query.MessageSummary{
				{
					ID:        1,
					Subject:   "First",
					FromEmail: "alice@example.com",
					SentAt:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				},
				{
					ID:        2,
					Subject:   "Second",
					FromEmail: "bob@test.org",
					SentAt:    time.Date(2024, 1, 16, 10, 30, 0, 0, time.UTC),
				},
			}, nil
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/search/domains?domains=example.com,%20test.org&after=2024-01-01&before=2024-02-01&limit=1&offset=3",
		nil,
	)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal([]string{"example.com", "test.org"}, gotDomains, "domains")
	require.NotNil(gotAfter, "after")
	assert.Equal("2024-01-01", gotAfter.Format("2006-01-02"), "after")
	require.NotNil(gotBefore, "before")
	assert.Equal("2024-02-01", gotBefore.Format("2006-01-02"), "before")
	assert.Equal(2, gotLimit, "fetch limit should probe one extra row")
	assert.Equal(3, gotOffset, "offset")

	var resp FilteredMessagesResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal(1, resp.Count, "count")
	assert.True(resp.HasMore, "has_more")
	assert.Equal(3, resp.Offset, "offset")
	assert.Equal(1, resp.Limit, "limit")
	require.Len(resp.Messages, 1, "messages")
	assert.Equal(int64(1), resp.Messages[0].ID, "message id")
}

func TestHandleFilteredMessagesIncludesDeletedAt(t *testing.T) {
	require := require.New(t)
	deletedAt := time.Date(2026, 3, 18, 15, 0, 0, 0, time.UTC)
	engine := &querytest.MockEngine{
		ListResults: []query.MessageSummary{
			{
				ID:        1,
				Subject:   "Deleted Email",
				FromEmail: "alice@example.com",
				SentAt:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				DeletedAt: &deletedAt,
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/filter?limit=100", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	messages, ok := resp["messages"].([]any)
	require.True(ok, "messages = %#v, want array", resp["messages"])
	require.Len(messages, 1, "messages count")

	message, ok := messages[0].(map[string]any)
	require.True(ok, "message = %#v, want object", messages[0])

	require.Equal(deletedAt.Format(time.RFC3339), message["deleted_at"], "deleted_at")
}

func TestHandleFilteredMessagesFormatsPhoneBackedSMSParticipants(t *testing.T) {
	require := require.New(t)
	engine := &querytest.MockEngine{
		ListResults: []query.MessageSummary{
			{
				ID:          1,
				Subject:     "",
				MessageType: "sms",
				FromName:    "SMS Sender",
				FromPhone:   "+15551234567",
				To:          []query.Address{{Email: "+15557654321", Name: "Me"}},
				SentAt:      time.Date(2024, 4, 1, 8, 0, 0, 0, time.UTC),
				Snippet:     "known sms snippet",
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/filter?message_type=sms&limit=1", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp struct {
		Messages []MessageSummary `json:"messages"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	require.Len(resp.Messages, 1, "messages count")
	require.Equal("SMS Sender <+15551234567>", resp.Messages[0].From, "from")
	require.Len(resp.Messages[0].To, 1, "to")
	require.Equal("Me <+15557654321>", resp.Messages[0].To[0], "to[0]")
}

func TestHandleFilteredMessagesFallsBackToContactNameWhenAddressMissing(t *testing.T) {
	require := require.New(t)
	engine := &querytest.MockEngine{
		ListResults: []query.MessageSummary{
			{
				ID:          1,
				Subject:     "",
				MessageType: "sms",
				FromName:    "ShortCode Alerts",
				// No email or phone — e.g. a short-code sender stored only via
				// participant_identifiers. Without the fallback the From
				// field would render empty.
				To:      []query.Address{{Name: "Me"}},
				SentAt:  time.Date(2024, 4, 1, 8, 0, 0, 0, time.UTC),
				Snippet: "your code is 123456",
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/filter?message_type=sms&limit=1", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp struct {
		Messages []MessageSummary `json:"messages"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	require.Len(resp.Messages, 1, "messages count")
	require.Equal("ShortCode Alerts", resp.Messages[0].From, "from")
	require.Len(resp.Messages[0].To, 1, "to")
	require.Equal("Me", resp.Messages[0].To[0], "to[0]")
}

func TestHandleTotalStats(t *testing.T) {
	assert := assert.New(t)
	engine := &querytest.MockEngine{
		Stats: &query.TotalStats{
			MessageCount:              1000,
			ActiveMessageCount:        700,
			SourceDeletedMessageCount: 300,
			TotalSize:                 5000000,
			AttachmentCount:           100,
			AttachmentSize:            1000000,
			LabelCount:                10,
			AccountCount:              2,
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats/total", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp TotalStatsResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal(int64(1000), resp.MessageCount, "message_count")
	assert.Equal(int64(700), resp.ActiveMessages, "active_messages")
	assert.Equal(int64(300), resp.SourceDeletedMessages, "source_deleted_messages")
	assert.Equal(int64(5000000), resp.TotalSize, "total_size")
}

func TestHandleFastSearch(t *testing.T) {
	assert := assert.New(t)
	engine := &querytest.MockEngine{
		SearchFastResults: []query.MessageSummary{
			{
				ID:        1,
				Subject:   "Invoice 12345",
				FromEmail: "billing@example.com",
				SentAt:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			},
		},
		Stats: &query.TotalStats{
			MessageCount: 1,
			TotalSize:    1024,
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/fast?q=invoice", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp SearchFastResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal("invoice", resp.Query, "query")
	assert.Len(resp.Messages, 1, "messages count")
}

func TestHandleFastSearchMissingQuery(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/fast", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "status")
}

func TestHandleFastSearchInvalidViewType(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/fast?q=test&view_type=invalid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "status")

	var errResp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "failed to decode error response")

	assert.Equal(t, "invalid_view_type", errResp["error"], "error")
}

// TestSearchRejectsMessageTypeFilterParam guards against silently dropping
// the message_type filter parameter. Fast/deep search support the parsed
// message_type: operator, but parseMessageFilter's parameter form is still
// list-search-only and must not be accepted as a no-op.
func TestSearchRejectsMessageTypeFilterParam(t *testing.T) {
	for _, path := range []string{
		"/api/v1/search/fast?q=hello&message_type=sms",
		"/api/v1/search/deep?q=hello&message_type=sms",
	} {
		t.Run(path, func(t *testing.T) {
			engine := &querytest.MockEngine{}
			srv := newTestServerWithEngine(t, engine)

			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()

			srv.Router().ServeHTTP(w, req)

			require.Equal(t, http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())
			var errResp map[string]string
			require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "decode error")
			require.Equal(t, "unsupported_filter", errResp["error"], "error")
		})
	}
}

func TestSearchParsedMessageTypeFilterReachesEngine(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "fast", path: "/api/v1/search/fast?q=message_type:sms%20hello"},
		{name: "deep", path: "/api/v1/search/deep?q=message_type:sms%20hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &querytest.MockEngine{
				Stats: &query.TotalStats{},
				SearchFastWithStatsFunc: func(_ context.Context, q *search.Query, _ string, _ query.MessageFilter, _ query.ViewType, _, _ int) (*query.SearchFastResult, error) {
					assert.Equal(t, []string{"sms"}, q.MessageTypes, "fast MessageTypes")
					return &query.SearchFastResult{Stats: &query.TotalStats{}}, nil
				},
				SearchFunc: func(_ context.Context, q *search.Query, _, _ int) ([]query.MessageSummary, error) {
					assert.Equal(t, []string{"sms"}, q.MessageTypes, "deep MessageTypes")
					return nil, nil
				},
			}
			srv := newTestServerWithEngine(t, engine)

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()

			srv.Router().ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
		})
	}
}

// TestFastDeepSearchRejectInvalidOperatorValue verifies the fast and deep
// search endpoints reject a query with an invalid known-operator value with a
// 400 invalid_query instead of silently dropping the operator and running a
// widened query. Regression coverage for the fast/deep gap in the CLI/API
// search validation.
func TestFastDeepSearchRejectInvalidOperatorValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "fast_bad_date", path: "/api/v1/search/fast?q=" + url.QueryEscape("before:not-a-date")},
		{name: "fast_bad_size", path: "/api/v1/search/fast?q=" + url.QueryEscape("larger:5X")},
		{name: "deep_bad_date", path: "/api/v1/search/deep?q=" + url.QueryEscape("before:not-a-date")},
		{name: "deep_bad_size", path: "/api/v1/search/deep?q=" + url.QueryEscape("larger:5X")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			var searchRan, fastRan bool
			engine := &querytest.MockEngine{
				Stats: &query.TotalStats{},
				SearchFunc: func(_ context.Context, _ *search.Query, _, _ int) ([]query.MessageSummary, error) {
					searchRan = true
					return nil, nil
				},
				SearchFastWithStatsFunc: func(_ context.Context, _ *search.Query, _ string, _ query.MessageFilter, _ query.ViewType, _, _ int) (*query.SearchFastResult, error) {
					fastRan = true
					return &query.SearchFastResult{Stats: &query.TotalStats{}}, nil
				},
			}
			srv := newTestServerWithEngine(t, engine)

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			require.Equal(http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())
			var resp ErrorResponse
			require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode error")
			assert.Equal("invalid_query", resp.Error, "error code")
			assert.False(searchRan, "deep search engine must not run")
			assert.False(fastRan, "fast search engine must not run")
		})
	}
}

// TestFastDeepSearchContextErrorReturns503 verifies that a context
// deadline/cancellation from the engine surfaces as a structured 503 rather
// than a generic 500 on the fast and deep search endpoints.
func TestFastDeepSearchContextErrorReturns503(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "fast", path: "/api/v1/search/fast?q=invoice", wantErr: "query_timeout"},
		{name: "deep", path: "/api/v1/search/deep?q=invoice", wantErr: "query_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			engine := &querytest.MockEngine{
				SearchFunc: func(_ context.Context, _ *search.Query, _, _ int) ([]query.MessageSummary, error) {
					return nil, context.DeadlineExceeded
				},
				SearchFastWithStatsFunc: func(_ context.Context, _ *search.Query, _ string, _ query.MessageFilter, _ query.ViewType, _, _ int) (*query.SearchFastResult, error) {
					return nil, context.DeadlineExceeded
				},
			}
			srv := newTestServerWithEngine(t, engine)

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			require.Equal(http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
			var resp ErrorResponse
			require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode error")
			assert.Equal(tc.wantErr, resp.Error, "error code")
		})
	}
}

// contextErrorTextEngine implements query.TextEngine with every method
// failing the same way a saturated DuckDB query slot does: a wrapped
// context error from acquireQuerySlot.
type contextErrorTextEngine struct {
	*querytest.MockEngine

	err error
}

func (e *contextErrorTextEngine) ListConversations(context.Context, query.TextFilter) ([]query.ConversationRow, error) {
	return nil, fmt.Errorf("acquire query slot: %w", e.err)
}

func (e *contextErrorTextEngine) TextAggregate(context.Context, query.TextViewType, query.TextAggregateOptions) ([]query.AggregateRow, error) {
	return nil, fmt.Errorf("acquire query slot: %w", e.err)
}

func (e *contextErrorTextEngine) ListConversationMessages(context.Context, int64, query.TextFilter) ([]query.MessageSummary, error) {
	return nil, fmt.Errorf("acquire query slot: %w", e.err)
}

func (e *contextErrorTextEngine) TextSearch(context.Context, string, int, int) ([]query.MessageSummary, error) {
	return nil, fmt.Errorf("acquire query slot: %w", e.err)
}

func (e *contextErrorTextEngine) GetTextStats(context.Context, query.TextStatsOptions) (*query.TotalStats, error) {
	return nil, fmt.Errorf("acquire query slot: %w", e.err)
}

// TestTextEndpointsContextErrorReturns503 verifies that a context
// deadline/cancellation from the text engine (e.g. a request timing out
// while waiting for a DuckDB query slot) surfaces as a structured 503
// instead of a generic 500 on every text endpoint.
func TestTextEndpointsContextErrorReturns503(t *testing.T) {
	paths := []struct {
		name string
		path string
	}{
		{name: "conversations", path: "/api/v1/text/conversations"},
		{name: "aggregates", path: "/api/v1/text/aggregates?view_type=contacts"},
		{name: "conversation messages", path: "/api/v1/text/conversations/1/messages"},
		{name: "search", path: "/api/v1/text/search?q=hello"},
		{name: "stats", path: "/api/v1/text/stats"},
	}
	cases := []struct {
		name    string
		err     error
		wantErr string
	}{
		{name: "deadline", err: context.DeadlineExceeded, wantErr: "query_timeout"},
		{name: "canceled", err: context.Canceled, wantErr: "query_canceled"},
	}
	for _, tc := range cases {
		for _, p := range paths {
			t.Run(tc.name+"/"+p.name, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				engine := &contextErrorTextEngine{MockEngine: &querytest.MockEngine{}, err: tc.err}
				srv := newTestServerWithEngine(t, engine)

				req := httptest.NewRequest(http.MethodGet, p.path, nil)
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, req)

				require.Equal(http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
				var resp ErrorResponse
				require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode error")
				assert.Equal(tc.wantErr, resp.Error, "error code")
			})
		}
	}
}

// TestSubAggregatesAcceptsAggregateSort verifies the sub-aggregate endpoint
// honors aggregate sort values (count, name, attachment_size) instead of
// rejecting them via the message-filter parser's message-sort validation.
func TestSubAggregatesAcceptsAggregateSort(t *testing.T) {
	for _, sortVal := range []string{"count", "name", "attachment_size", "size"} {
		t.Run(sortVal, func(t *testing.T) {
			require := require.New(t)
			engine := &querytest.MockEngine{}
			srv := newTestServerWithEngine(t, engine)

			req := httptest.NewRequest(http.MethodGet,
				"/api/v1/aggregates/sub?view_type=labels&sender=alice@example.com&sort="+sortVal, nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
		})
	}
}

func TestRemoteSearchParsedMessageTypeThroughAPI(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	engine := &querytest.MockEngine{
		SearchFunc: func(_ context.Context, q *search.Query, _, _ int) ([]query.MessageSummary, error) {
			assert.Equal([]string{"sms"}, q.MessageTypes, "MessageTypes")
			assert.Equal([]string{"hello"}, q.TextTerms, "TextTerms")
			return []query.MessageSummary{{
				ID:          7,
				Subject:     "hello",
				MessageType: "sms",
				SentAt:      time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			}}, nil
		},
		SearchFastWithStatsFunc: func(_ context.Context, q *search.Query, _ string, _ query.MessageFilter, _ query.ViewType, _, _ int) (*query.SearchFastResult, error) {
			assert.Equal([]string{"sms"}, q.MessageTypes, "fast MessageTypes")
			assert.Equal([]string{"hello"}, q.TextTerms, "fast TextTerms")
			return &query.SearchFastResult{
				Messages: []query.MessageSummary{{
					ID:          8,
					Subject:     "hello fast",
					MessageType: "sms",
					SentAt:      time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				}},
				TotalCount: 1,
				Stats:      &query.TotalStats{MessageCount: 1},
			}, nil
		},
	}
	srv := newTestServerWithEngine(t, engine)
	httpSrv := httptest.NewServer(srv.Router())
	defer httpSrv.Close()

	remoteEngine, err := daemonclient.NewEngine(daemonclient.Config{
		URL:           httpSrv.URL,
		AllowInsecure: true,
	})
	require.NoError(err, "NewEngine")

	results, err := remoteEngine.Search(context.Background(), &search.Query{
		TextTerms:    []string{"hello"},
		MessageTypes: []string{"sms"},
	}, 10, 0)
	require.NoError(err, "remote Search")
	require.Len(results, 1, "results")
	assert.Equal("sms", results[0].MessageType, "MessageType")

	fastResults, err := remoteEngine.SearchFast(context.Background(), &search.Query{
		TextTerms:    []string{"hello"},
		MessageTypes: []string{"sms"},
	}, query.MessageFilter{}, 10, 0)
	require.NoError(err, "remote SearchFast")
	require.Len(fastResults, 1, "fast results")
	assert.Equal("sms", fastResults[0].MessageType, "fast MessageType")
}

func TestHandleDeepSearch(t *testing.T) {
	engine := &querytest.MockEngine{
		SearchResults: []query.MessageSummary{
			{
				ID:        1,
				Subject:   "Meeting Notes",
				FromEmail: "team@example.com",
				SentAt:    time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/deep?q=agenda", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal(t, "agenda", resp["query"], "query")
}

type bodySearchTestEngine struct {
	*querytest.MockEngine

	searchMessageBodiesFunc func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error)
}

func (e *bodySearchTestEngine) SearchMessageBodies(
	ctx context.Context, q *search.Query, limit, offset int,
) ([]query.MessageSummary, error) {
	return e.searchMessageBodiesFunc(ctx, q, limit, offset)
}

func TestHandleDeepSearchBodyScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var genericCalled, bodyCalled bool
	engine := &bodySearchTestEngine{
		MockEngine: &querytest.MockEngine{
			SearchFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
				genericCalled = true
				return []query.MessageSummary{{ID: 1, Subject: "generic false positive"}}, nil
			},
		},
		searchMessageBodiesFunc: func(_ context.Context, q *search.Query, limit, offset int) ([]query.MessageSummary, error) {
			bodyCalled = true
			assert.Equal([]string{"bodyneedle"}, q.TextTerms, "body TextTerms")
			assert.Equal(11, limit, "limit includes has_more probe")
			assert.Equal(3, offset, "offset")
			return []query.MessageSummary{{
				ID:                           2,
				Subject:                      "body hit",
				BodyContextSnippets:          []string{"exact body context"},
				BodyContextSnippetsTruncated: true,
			}}, nil
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/search/deep?q=bodyneedle&scope=body&limit=10&offset=3", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.False(genericCalled, "generic Search must not handle scope=body")
	assert.True(bodyCalled, "SearchMessageBodies must handle scope=body")
	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Equal("body", resp["scope"], "echoed scope")
	messages, ok := resp["messages"].([]any)
	require.True(ok, "messages response type")
	require.Len(messages, 1)
	message, ok := messages[0].(map[string]any)
	require.True(ok, "message response type")
	assert.InDelta(float64(2), message["id"], 0, "body hit ID")
	assert.NotContains(message, "context_snippets",
		"body search must preserve the shared MessageSummary schema")
	assert.NotContains(message, "context_snippets_truncated")
	contexts, ok := resp["body_contexts"].([]any)
	require.True(ok, "body_contexts response type")
	require.Len(contexts, 1)
	bodyContext, ok := contexts[0].(map[string]any)
	require.True(ok, "body context response type")
	assert.InDelta(float64(2), bodyContext["message_id"], 0, "body context message ID")
	assert.Equal([]any{"exact body context"}, bodyContext["context_snippets"])
	assert.Equal(true, bodyContext["context_snippets_truncated"])
}

func TestHandleDeepSearchScopeValidation(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		engine     query.Engine
		wantStatus int
	}{
		{
			name:       "invalid scope",
			path:       "/api/v1/search/deep?q=needle&scope=metadata",
			engine:     &querytest.MockEngine{},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "body scope requires free text",
			path:       "/api/v1/search/deep?q=from%3Aalice%40example.com&scope=body",
			engine:     &bodySearchTestEngine{MockEngine: &querytest.MockEngine{}, searchMessageBodiesFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) { return nil, nil }},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "body capability unavailable",
			path:       "/api/v1/search/deep?q=needle&scope=body",
			engine:     struct{ query.Engine }{Engine: &querytest.MockEngine{}},
			wantStatus: http.StatusNotImplemented,
		},
		{
			name: "body work limit is a client error",
			path: "/api/v1/search/deep?q=needle&scope=body",
			engine: &bodySearchTestEngine{
				MockEngine: &querytest.MockEngine{},
				searchMessageBodiesFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
					return nil, fmt.Errorf("%w: supports at most 32 free-text terms", query.ErrMessageBodySearchInvalidQuery)
				},
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServerWithEngine(t, tc.engine)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			assert.Equal(t, tc.wantStatus, w.Code, "status (body: %s)", w.Body.String())
		})
	}
}

func TestHandleDeepSearchBodyReadinessErrorIsActionable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	engine := &bodySearchTestEngine{
		MockEngine: &querytest.MockEngine{},
		searchMessageBodiesFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
			return nil, fmt.Errorf("%w: run 'msgvault rebuild-fts' or complete the FTS backfill", query.ErrMessageBodySearchIndexStale)
		},
	}
	srv := newTestServerWithEngine(t, engine)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/deep?q=needle&scope=body", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode error")
	assert.Equal("body_search_index_unavailable", resp.Error, "error code")
	assert.Contains(resp.Message, "rebuild-fts", "actionable rebuild guidance")
	assert.Contains(resp.Message, "backfill", "actionable backfill guidance")
}

func TestHandleDeepSearchOmittedScopeUsesGenericSearch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var genericCalled bool
	engine := &bodySearchTestEngine{
		MockEngine: &querytest.MockEngine{
			SearchFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
				genericCalled = true
				return nil, nil
			},
		},
		searchMessageBodiesFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
			assert.Fail("SearchMessageBodies must not handle omitted scope")
			return nil, nil
		},
	}
	srv := newTestServerWithEngine(t, engine)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/deep?q=needle", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.True(genericCalled, "generic Search handles omitted scope")
	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.NotContains(resp, "scope", "omitted scope stays generic")
}

func TestHandleDeepSearchMissingQuery(t *testing.T) {
	engine := &querytest.MockEngine{}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/deep", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "status")
}

// mockSQLQueryEngine wraps MockEngine and adds SQLQuerier support.
type mockSQLQueryEngine struct {
	querytest.MockEngine

	queryResult *query.QueryResult
	queryErr    error
}

func (m *mockSQLQueryEngine) QuerySQL(_ context.Context, _ string) (*query.QueryResult, error) {
	return m.queryResult, m.queryErr
}

func TestHandleQuery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	engine := &mockSQLQueryEngine{
		queryResult: &query.QueryResult{
			Columns:  []string{"from_email"},
			Rows:     [][]any{{"alice@example.com"}},
			RowCount: 1,
		},
	}

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg,
		Engine: engine,
		Logger: testLogger(),
	})

	body := `{"sql": "SELECT from_email FROM v_senders LIMIT 1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var result query.QueryResult
	require.NoError(json.NewDecoder(w.Body).Decode(&result), "failed to decode response")

	assert.Equal(1, result.RowCount, "row_count")
	require.Len(result.Columns, 1, "columns")
	assert.Equal("from_email", result.Columns[0], "columns[0]")
}

func TestHandleQueryUsesConfiguredRunnerWhenEngineDoesNotSupportSQL(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	var gotSQL string
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg,
		Engine: &querytest.MockEngine{},
		Logger: testLogger(),
		SQLQueryRunner: func(_ context.Context, sql string) (*query.QueryResult, error) {
			gotSQL = sql
			return &query.QueryResult{
				Columns:  []string{"subject"},
				Rows:     [][]any{{"Hello"}},
				RowCount: 1,
			}, nil
		},
	})

	body := `{"sql": "SELECT subject FROM messages LIMIT 1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal("SELECT subject FROM messages LIMIT 1", gotSQL, "sql")

	var result query.QueryResult
	require.NoError(json.NewDecoder(w.Body).Decode(&result), "decode response")
	assert.Equal([]string{"subject"}, result.Columns, "columns")
	assert.Equal(1, result.RowCount, "row_count")
}

func TestHandleSearch_FTSModeUnchanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, _ := newTestServerWithMockStore(t)

	// mode=fts (or unset) should still return the legacy SearchResult shape.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=Test&mode=fts", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp SearchResult
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")
	assert.Equal("Test", resp.Query, "query")
	// Legacy shape exposes page + page_size; hybrid shape would not.
	assert.NotZero(resp.Page, "expected page field in FTS response")
}

func TestHandleSearch_HybridModeNotConfigured(t *testing.T) {
	// newTestServerWithMockStore does not inject a HybridEngine, so
	// the server must return 503 for any vector/hybrid query.
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=test&mode=hybrid", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "failed to decode error response")
	assert.Equal(t, "vector_not_enabled", errResp.Error, "error")
}

// newHybridServerForErrorTest constructs an API server with a real
// hybrid.Engine wired around a fakeVectorBackend in the supplied state.
// mainDB is nil because the test queries used here have no operators,
// so BuildFilter never touches it.
func newHybridServerForErrorTest(t *testing.T, backend vector.Backend) *Server {
	t.Helper()
	engine := hybrid.NewEngine(backend, nil, stubEmbedder{}, hybrid.Config{
		ExpectedFingerprint: "nomic-embed:768",
		RRFK:                60,
		KPerSignal:          10,
	})
	cfg := &config.Config{Server: config.ServerConfig{APIPort: 8080}}
	return NewServerWithOptions(ServerOptions{
		Config:       cfg,
		Store:        &mockStore{},
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})
}

// TestHandleSearch_HybridErrIndexBuilding regression-guards the API's
// translation of vector.ErrIndexBuilding from the hybrid engine into a
// 503 with error code "index_building". The engine returns this when
// no active generation exists yet but a build is in progress.
func TestHandleSearch_HybridErrIndexBuilding(t *testing.T) {
	building := &vector.Generation{
		ID: 1, Model: "nomic-embed", Dimension: 768,
		Fingerprint: "nomic-embed:768", State: vector.GenerationBuilding,
	}
	backend := &fakeVectorBackend{building: building}
	srv := newHybridServerForErrorTest(t, backend)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=anything&mode=hybrid", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "decode")
	assert.Equal(t, "index_building", errResp.Error, "error")
}

// TestHandleSearch_HybridErrNotEnabled regression-guards the API's
// translation of vector.ErrNotEnabled from the hybrid engine into a
// 503 with error code "vector_not_enabled". The engine returns this
// when no generation exists at all (no active, no building).
func TestHandleSearch_HybridErrNotEnabled(t *testing.T) {
	backend := &fakeVectorBackend{} // no active, no building
	srv := newHybridServerForErrorTest(t, backend)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=anything&mode=hybrid", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "decode")
	assert.Equal(t, "vector_not_enabled", errResp.Error, "error")
}

// realEmbedder returns a deterministic single vector per input. Used
// when the test exercises the end-to-end engine path (mode=vector hits
// backend.Search, which requires a query vector to have been produced).
type realEmbedder struct {
	dim int
}

func (e realEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		v := make([]float32, e.dim)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

// blockingEmbedder waits for ctx cancellation, then returns ctx.Err().
// Used to simulate a slow embedder that must be cancelled by the
// request-scoped timeout to terminate.
type blockingEmbedder struct{}

func (blockingEmbedder) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestHandleSearch_HybridEmbeddingTimeoutReturnsStructuredError regresses the
// concern that request timeout handling could preempt our structured 503
// embedding_timeout response. The server timeout middleware must cancel the
// request context without using http.TimeoutHandler-style preemption. The
// handler sees ctx.DeadlineExceeded from the embed call, the engine wraps it as
// vector.ErrEmbeddingTimeout, and the handler writes 503 embedding_timeout JSON.
//
// The test sets a tight RequestTimeout so cancellation fires during the embed
// call. If http.TimeoutHandler-style preemption is introduced, this test will
// fail because the response would be a bare 504 instead of the structured 503.
func TestHandleSearch_HybridEmbeddingTimeoutReturnsStructuredError(t *testing.T) {
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
	}
	engine := hybrid.NewEngine(backend, nil, blockingEmbedder{}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:         &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:          &mockStore{},
		HybridEngine:   engine,
		Backend:        backend,
		Logger:         testLogger(),
		RequestTimeout: 100 * time.Millisecond,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=meeting&mode=hybrid", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "decode")
	assert.Equal(t, "embedding_timeout", errResp.Error,
		"error (timeout handling may have preempted with a bare 504)")
}

// TestHandleSearch_HybridFilterOnlyReturnsBadRequest regression-guards
// the spec contract that mode=vector|hybrid requires at least one
// free-text term to embed. A query containing only operators (e.g.
// `from:alice@example.com`) parses to an empty TextTerms slice; the
// handler must reject this with 400 missing_free_text rather than
// passing an empty string into the embedder.
func TestHandleSearch_HybridFilterOnlyReturnsBadRequest(t *testing.T) {
	// Construct a real engine so the handler progresses past the
	// "vector_not_enabled" check before evaluating freeText.
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
	}
	engine := hybrid.NewEngine(backend, nil, stubEmbedder{}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        &mockStore{},
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/search?q=from:alice@example.com&mode=vector", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "decode")
	assert.Equal(t, "missing_free_text", errResp.Error, "error")
}

func TestEmbeddingBodyText_HTMLOnlyUsesEmbeddingCorpus(t *testing.T) {
	msg := &APIMessage{
		Body:     "<p>semantic <strong>needle</strong></p>",
		BodyHTML: "<p>semantic <strong>needle</strong></p>",
	}

	assert.Equal(t, "semantic needle", embeddingBodyText(msg))
}

func TestHandleSearch_VectorMessageTypeParamReachesFilter(t *testing.T) {
	store := &mockStore{
		messages: []APIMessage{{
			ID:          42,
			Subject:     "Lunch",
			MessageType: "sms",
			Snippet:     "grab lunch",
		}},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		searchHits: []vector.Hit{{MessageID: 42, Score: 0.9, Rank: 1}},
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/search?q=lunch&mode=vector&message_type=sms", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal(t, []string{"sms"}, backend.searchFilter.MessageTypes, "MessageTypes")
}

func TestHandleSearch_VectorAccountParamReachesFilter(t *testing.T) {
	store := &mockStore{
		messages: []APIMessage{{ID: 42, Subject: "Lunch"}},
		sourcesByLookup: map[string][]*store.Source{
			"alice@example.com": {
				{ID: 77, SourceType: "gmail", Identifier: "alice@example.com"},
			},
		},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		searchHits: []vector.Hit{{MessageID: 42, Score: 0.9, Rank: 1}},
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/search?q=lunch&mode=vector&account=alice@example.com", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal(t, []int64{77}, backend.searchFilter.SourceIDs, "SourceIDs")
}

func TestHandleSearch_VectorCollectionParamReachesFilter(t *testing.T) {
	store := &mockStore{
		messages: []APIMessage{{ID: 42, Subject: "Lunch"}},
		collections: map[string]*store.CollectionWithSources{
			"Important": {
				Collection: store.Collection{
					ID:   3,
					Name: "Important",
				},
				SourceIDs: []int64{77, 88},
			},
		},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		searchHits: []vector.Hit{{MessageID: 42, Score: 0.9, Rank: 1}},
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/search?q=lunch&mode=vector&collection=Important", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal(t, []int64{77, 88}, backend.searchFilter.SourceIDs, "SourceIDs")
}

// TestHandleSearch_HybridResponseItemShape regression-guards the
// hybrid response item shape: each result must be a MessageSummary
// (snake-case fields shared with /api/v1/search FTS mode), not a
// bespoke object that diverges from the legacy summary surface.
// Catches regressions where the embedded type or omitempty rules
// drift away from MessageSummary.
func TestHandleSearch_HybridResponseItemShape(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	deletedAt := time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC)
	store := &mockStore{
		messages: []APIMessage{{
			ID:             42,
			ConversationID: 7,
			Subject:        "Quarterly Plan",
			From:           "Alice <alice@example.com>",
			FromEmail:      "alice@example.com",
			FromName:       "Alice",
			To:             []string{"bob@example.com"},
			Cc:             []string{"carol@example.com"},
			SentAt:         time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			DeletedAt:      &deletedAt,
			Snippet:        "discussion of Q1 priorities",
			Labels:         []string{"INBOX", "Work"},
			HasAttachments: true,
			SizeEstimate:   2048,
		}},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		searchHits: []vector.Hit{{MessageID: 42, Score: 0.9, Rank: 1}},
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=quarterly&mode=vector", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	// Decode into a raw map so we can verify field names (snake-case
	// keys) and that no unexpected wrapper is present.
	var resp struct {
		Mode    string           `json:"mode"`
		Results []map[string]any `json:"results"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.Equal("vector", resp.Mode, "mode")
	require.Len(resp.Results, 1, "results len")
	got := resp.Results[0]
	// Required MessageSummary fields. Score must be ABSENT (no explain=1).
	wantKeys := []string{
		"id", "conversation_id", "subject", "from", "to", "cc",
		"from_email", "from_name",
		"sent_at", "deleted_at", "snippet", "labels",
		"has_attachments", "size_bytes",
	}
	for _, k := range wantKeys {
		assert.Contains(got, k, "missing key %q in hybrid result item, got keys: %v", k, mapKeys(got))
	}
	assert.NotContains(got, "score", "'score' key present without explain=1, got %v", got["score"])
	// Spot-check a couple of values to make sure it's the same message.
	id, _ := got["id"].(float64)
	assert.Equal(int64(42), int64(id), "id")
	subj, _ := got["subject"].(string)
	assert.Equal("Quarterly Plan", subj, "subject")
	fromEmail, _ := got["from_email"].(string)
	assert.Equal("alice@example.com", fromEmail, "from_email")
	fromName, _ := got["from_name"].(string)
	assert.Equal("Alice", fromName, "from_name")
	hasA, _ := got["has_attachments"].(bool)
	assert.True(hasA, "has_attachments")
}

func TestHandleSearch_VectorExplainAcceptsBooleanQueryValue(t *testing.T) {
	require := require.
		New(t)

	store := &mockStore{
		messages: []APIMessage{{
			ID:             42,
			ConversationID: 7,
			Subject:        "Quarterly Plan",
			FromEmail:      "alice@example.com",
			SentAt:         time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		}},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		searchHits: []vector.Hit{{MessageID: 42, Score: 0.9, Rank: 1}},
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=quarterly&mode=vector&explain=true", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp struct {
		Results []map[string]any `json:"results"`
	}
	require.NoError(
		json.NewDecoder(w.Body).Decode(&resp), "decode")

	require.Len(resp.Results, 1, "results")
	assert.Contains(t, resp.Results[0], "score", "explain=true should include score breakdown")
}

// TestHandleSearch_HybridUsesBulkHydration regresses the N+1 bug
// where each hit triggered its own GetMessage call (which fetches
// body, all four recipient sets, labels, and attachments per id —
// roughly 7 queries per hit). Hybrid search must instead make a
// single GetMessagesSummariesByIDs call carrying every hit's id.
func TestHandleSearch_HybridUsesBulkHydration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := &mockStore{
		messages: []APIMessage{
			{ID: 1, Subject: "first", From: "a@x", Snippet: "..."},
			{ID: 2, Subject: "second", From: "b@x", Snippet: "..."},
			{ID: 3, Subject: "third", From: "c@x", Snippet: "..."},
		},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		searchHits: []vector.Hit{
			{MessageID: 1, Score: 0.9, Rank: 1},
			{MessageID: 2, Score: 0.8, Rank: 2},
			{MessageID: 3, Score: 0.7, Rank: 3},
		},
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=test&mode=vector", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal(int32(0), store.getMessageCalls.Load(),
		"GetMessage call count, want 0 (must use bulk hydration, not per-hit)")
	assert.Equal(int32(1), store.getSummariesByIDsCalls.Load(),
		"GetMessagesSummariesByIDs call count, want 1 (single bulk lookup)")
	wantIDs := []int64{1, 2, 3}
	require.Equal(wantIDs, store.getSummariesByIDsLastIDs, "getSummariesByIDs last ids")
}

func TestHandleSearch_VectorMatchEnrichmentIsOptInAndPageAware(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := &mockStore{
		messages: []APIMessage{
			{ID: 1, Subject: "first", Body: "first body"},
			{ID: 2, Subject: "second", Body: "second body"},
			{ID: 3, Subject: "third", Body: "third body"},
			{ID: 4, Subject: "probe", Body: "probe body"},
		},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		searchHits: []vector.Hit{
			{MessageID: 1, Score: 0.95, Rank: 1},
			{MessageID: 2, Score: 0.90, Rank: 2},
			{MessageID: 3, Score: 0.85, Rank: 3},
			{MessageID: 4, Score: 0.80, Rank: 4},
		},
		chunkHits: map[int64][]vector.ChunkHit{
			2: {{ChunkCharStart: 0, ChunkCharEnd: 11, Score: 0.9}},
			3: {{ChunkCharStart: 0, ChunkCharEnd: 10, Score: 0.7}},
			4: {{ChunkCharStart: 0, ChunkCharEnd: 10, Score: 0.95}},
		},
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/search?q=body&mode=vector&offset=1&page_size=2&include_matches=true&min_score=0.8", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp struct {
		HasMore bool `json:"has_more"`
		Results []struct {
			ID      int64 `json:"id"`
			Matches []struct {
				Snippet string  `json:"snippet"`
				Score   float64 `json:"score"`
			} `json:"matches"`
		} `json:"results"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.True(resp.HasMore, "probe hit should set has_more")
	require.Len(resp.Results, 2, "page results")
	assert.Equal(int64(2), resp.Results[0].ID, "first page id")
	assert.Equal(int64(3), resp.Results[1].ID, "second page id")
	require.Len(resp.Results[0].Matches, 1, "high-score excerpts")
	assert.InDelta(0.9, resp.Results[0].Matches[0].Score, 0.001, "match score")
	assert.Empty(resp.Results[1].Matches, "low-score excerpts")
	assert.Equal([]int64{2, 3}, store.getSummariesByIDsLastIDs, "summary hydration ids")
	assert.Equal(int32(2), store.getMessageCalls.Load(), "body hydration calls")
	assert.Equal([]int64{2, 3}, backend.chunkScoreIDs, "chunk scoring ids")
	assert.Equal(4, backend.searchLimit, "ranking prefix includes one probe")
}

func TestHandleSimilarSearchUsesVectorBackend(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := &mockStore{
		messages: []APIMessage{
			{ID: 1, Subject: "seed", From: "seed@example.com", Snippet: "..."},
			{ID: 2, Subject: "second", From: "a@example.com", Snippet: "..."},
			{ID: 3, Subject: "third", From: "b@example.com", Snippet: "..."},
		},
	}
	cfg := vector.Config{
		Embeddings: vector.EmbeddingsConfig{Model: "fake", Dimension: 4},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 7, Model: "fake", Dimension: 4,
			Fingerprint: cfg.GenerationFingerprint(), State: vector.GenerationActive,
		},
		loadVec: []float32{1, 0, 0, 0},
		searchHits: []vector.Hit{
			{MessageID: 1, Score: 1, Rank: 1},
			{MessageID: 2, Score: 0.9, Rank: 2},
			{MessageID: 3, Score: 0.8, Rank: 3},
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:    &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:     store,
		VectorCfg: cfg,
		Backend:   backend,
		Logger:    testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=1&limit=2&message_type=sms&has_attachment=true", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal([]string{"sms"}, backend.searchFilter.MessageTypes, "message types")
	require.NotNil(backend.searchFilter.HasAttachment, "has attachment filter")
	assert.True(*backend.searchFilter.HasAttachment, "has attachment filter")
	assert.Equal(int32(1), store.getSummariesByIDsCalls.Load(), "bulk hydration calls")
	assert.Equal([]int64{2, 3}, store.getSummariesByIDsLastIDs, "hydrated ids")

	var resp struct {
		SeedMessageID int64 `json:"seed_message_id"`
		Returned      int   `json:"returned"`
		Generation    struct {
			ID int64 `json:"id"`
		} `json:"generation"`
		Messages []MessageSummary `json:"messages"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.Equal(int64(1), resp.SeedMessageID, "seed_message_id")
	assert.Equal(2, resp.Returned, "returned")
	assert.Equal(int64(7), resp.Generation.ID, "generation id")
	require.Len(resp.Messages, 2, "messages")
	assert.Equal(int64(2), resp.Messages[0].ID, "first result")
	assert.Equal(int64(3), resp.Messages[1].ID, "second result")
}

func TestHandleSimilarSearchHasAttachmentFalseDoesNotFilter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := &mockStore{
		messages: []APIMessage{
			{ID: 1, Subject: "seed", From: "seed@example.com", Snippet: "..."},
			{ID: 2, Subject: "second", From: "a@example.com", Snippet: "..."},
		},
	}
	cfg := vector.Config{
		Embeddings: vector.EmbeddingsConfig{Model: "fake", Dimension: 4},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 7, Model: "fake", Dimension: 4,
			Fingerprint: cfg.GenerationFingerprint(), State: vector.GenerationActive,
		},
		loadVec: []float32{1, 0, 0, 0},
		searchHits: []vector.Hit{
			{MessageID: 1, Score: 1, Rank: 1},
			{MessageID: 2, Score: 0.9, Rank: 2},
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:    &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:     store,
		VectorCfg: cfg,
		Backend:   backend,
		Logger:    testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=1&has_attachment=false", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Nil(backend.searchFilter.HasAttachment, "false leaves the attachment-only filter unset")
}

func TestHandleSimilarSearchRejectsInvalidHasAttachment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := NewServerWithOptions(ServerOptions{
		Config:    &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:     &mockStore{},
		VectorCfg: vector.Config{Embeddings: vector.EmbeddingsConfig{Model: "fake", Dimension: 4}},
		Backend:   &fakeVectorBackend{},
		Logger:    testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=1&has_attachment=maybe", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())
	var errResp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&errResp), "decode error response")
	assert.Equal("invalid_has_attachment", errResp.Error, "error")
	assert.Contains(errResp.Message, "has_attachment", "message names parameter")
}

// TestHandleStats_ContextErrorReturns503 verifies a stats read that overran
// its context budget surfaces as a structured 503, not a generic 500.
func TestHandleStats_ContextErrorReturns503(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := &mockStore{statsErr: context.DeadlineExceeded}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  store,
		Logger: testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.Equal("query_timeout", resp.Error, "error code")
}

// TestHandleSearch_HybridHydrationContextErrorReturns503 verifies that a
// canceled hydration on the hybrid path is surfaced as a structured 503
// instead of a 200 with missing results.
func TestHandleSearch_HybridHydrationContextErrorReturns503(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := &mockStore{
		messages: []APIMessage{
			{ID: 1, Subject: "first", From: "a@x", Snippet: "..."},
		},
		summariesErr: context.Canceled,
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		searchHits: []vector.Hit{{MessageID: 1, Score: 0.9, Rank: 1}},
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=test&mode=vector", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.Equal("query_canceled", resp.Error, "error code")
}

// TestHandleSimilarSearch_HydrationContextErrorReturns503 verifies the similar
// search path surfaces a canceled hydration as a structured 503.
func TestHandleSimilarSearch_HydrationContextErrorReturns503(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := &mockStore{
		messages: []APIMessage{
			{ID: 1, Subject: "seed", From: "seed@example.com", Snippet: "..."},
			{ID: 2, Subject: "second", From: "a@example.com", Snippet: "..."},
		},
		summariesErr: context.Canceled,
	}
	cfg := vector.Config{
		Embeddings: vector.EmbeddingsConfig{Model: "fake", Dimension: 4},
	}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 7, Model: "fake", Dimension: 4,
			Fingerprint: cfg.GenerationFingerprint(), State: vector.GenerationActive,
		},
		loadVec: []float32{1, 0, 0, 0},
		searchHits: []vector.Hit{
			{MessageID: 1, Score: 1, Rank: 1},
			{MessageID: 2, Score: 0.9, Rank: 2},
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:    &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:     store,
		VectorCfg: cfg,
		Backend:   backend,
		Logger:    testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=1&limit=2", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())
	var resp ErrorResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.Equal("query_canceled", resp.Error, "error code")
}

// TestHandleSearch_HybridPoolSaturatedAlwaysEmitted regression-guards
// the wire-level contract that pool_saturated is always present on a
// successful hybrid response (never omitted, never null). Without an
// explicit test, an `omitempty` slip on the response struct would
// silently drop the field for false values — clients that read
// "pool not saturated" as a positive signal would break.
func TestHandleSearch_HybridPoolSaturatedAlwaysEmitted(t *testing.T) {
	require := require.New(t)
	store := &mockStore{}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		// No hits → pool_saturated will be false (len(hits) < limit).
		searchHits: nil,
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	srv := NewServerWithOptions(ServerOptions{
		Config:       &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:        store,
		HybridEngine: engine,
		Backend:      backend,
		Logger:       testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=hello&mode=vector", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal(w.Body.Bytes(), &raw), "decode raw")
	val, exists := raw["pool_saturated"]
	require.True(exists, "pool_saturated key missing from successful response; want present (raw=%s)", w.Body.String())
	assert.Equal(t, "false", string(val), "pool_saturated")
}

// mapKeys returns the keys of a map[string]interface{} for use in
// assertion error messages.
func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestHandleSearch_HybridModePaginationUnsupported(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=test&mode=vector&page=2", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "failed to decode error response")
	assert.Equal(t, "pagination_unsupported", errResp.Error, "error")
}

func TestHandleSearch_UnknownMode(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=test&mode=bogus", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "failed to decode error response")
	assert.Equal(t, "invalid_mode", errResp.Error, "error")
}

func TestHandleQuery_SQLiteEngine503(t *testing.T) {
	engine := query.NewSQLiteEngine(nil)

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg,
		Engine: engine,
		Logger: testLogger(),
	})

	body := `{"sql": "SELECT 1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "status (body: %s)", w.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp), "failed to decode error response")
	assert.Equal(t, "engine_unavailable", errResp.Error, "error")
}

// fakeVectorBackend is a test stub implementing vector.Backend. Tests
// that need canned ANN hits set searchHits/searchErr; Stats and the
// generation-resolution paths use the other fields.
type fakeVectorBackend struct {
	active        *vector.Generation
	building      *vector.Generation
	stats         vector.Stats
	loadVec       []float32
	loadErr       error
	searchHits    []vector.Hit
	searchErr     error
	searchFilter  vector.Filter
	searchLimit   int
	chunkHits     map[int64][]vector.ChunkHit
	chunkScoreIDs []int64
}

func (f *fakeVectorBackend) CreateGeneration(_ context.Context, _ string, _ int, _ string) (vector.GenerationID, error) {
	return 0, errors.New("not implemented")
}
func (f *fakeVectorBackend) ActivateGeneration(_ context.Context, _ vector.GenerationID, _ bool) error {
	return errors.New("not implemented")
}
func (f *fakeVectorBackend) RetireGeneration(_ context.Context, _ vector.GenerationID, _ bool) error {
	return errors.New("not implemented")
}
func (f *fakeVectorBackend) ActiveGeneration(_ context.Context) (vector.Generation, error) {
	if f.active == nil {
		return vector.Generation{}, vector.ErrNoActiveGeneration
	}
	return *f.active, nil
}
func (f *fakeVectorBackend) BuildingGeneration(_ context.Context) (*vector.Generation, error) {
	return f.building, nil
}
func (f *fakeVectorBackend) Upsert(_ context.Context, _ vector.GenerationID, _ []vector.Chunk) error {
	return errors.New("not implemented")
}
func (f *fakeVectorBackend) Search(
	_ context.Context, _ vector.GenerationID, _ []float32, limit int, filter vector.Filter,
) ([]vector.Hit, error) {
	f.searchFilter = filter
	f.searchLimit = limit
	return f.searchHits, f.searchErr
}
func (f *fakeVectorBackend) ScoreMessageChunks(
	_ context.Context, _ vector.GenerationID, messageID int64, _ []float32,
) ([]vector.ChunkHit, error) {
	f.chunkScoreIDs = append(f.chunkScoreIDs, messageID)
	return f.chunkHits[messageID], nil
}
func (f *fakeVectorBackend) Delete(_ context.Context, _ vector.GenerationID, _ []int64) error {
	return errors.New("not implemented")
}
func (f *fakeVectorBackend) Stats(_ context.Context, _ vector.GenerationID) (vector.Stats, error) {
	return f.stats, nil
}
func (f *fakeVectorBackend) LoadVector(_ context.Context, _ int64) ([]float32, error) {
	return f.loadVec, f.loadErr
}
func (f *fakeVectorBackend) ResetWatermarkBelow(_ context.Context, _ int64) error {
	return nil
}
func (f *fakeVectorBackend) EmbeddedMessageCount(_ context.Context, _ vector.GenerationID) (int64, error) {
	return 0, errors.New("not implemented")
}
func (f *fakeVectorBackend) Close() error { return nil }

func TestHandleStats_VectorDisabled(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)
	// newTestServerWithMockStore uses NewServer (no Backend), so backend == nil.

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	// Parse raw JSON to verify "vector_search" is absent entirely.
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw), "decode")
	assert.NotContains(t, raw, "vector_search",
		"expected 'vector_search' to be absent from JSON when backend is nil")
}

func TestHandleStats_VectorEnabledWithActive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID:          5,
			Model:       "nomic-embed",
			Dimension:   768,
			Fingerprint: "nomic-embed:768",
			State:       vector.GenerationActive,
		},
		stats: vector.Stats{EmbeddingCount: 100, PendingCount: 7, StorageBytes: 4096},
	}

	store := &mockStore{
		stats: &StoreStats{
			MessageCount: 100,
			ThreadCount:  50,
			SourceCount:  1,
		},
	}
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     store,
		Backend:   backend,
		Scheduler: sched,
		Logger:    testLogger(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())

	var resp map[string]any
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")

	vs, ok := resp["vector_search"].(map[string]any)
	require.True(ok, "expected 'vector_search' object, got %T: %v", resp["vector_search"], resp["vector_search"])

	assert.Equal(true, vs["enabled"], "enabled")
	assert.InDelta(float64(7), vs["missing_embeddings_total"], 1e-9, "missing_embeddings_total")

	active, ok := vs["active_generation"].(map[string]any)
	require.True(ok, "expected 'vector_search.active_generation' object, got %T", vs["active_generation"])

	assert.InDelta(float64(100), active["message_count"], 1e-9, "message_count")
	assert.Equal("nomic-embed", active["model"], "model")
	assert.InDelta(float64(5), active["id"], 1e-9, "id")
	assert.InDelta(float64(768), active["dimension"], 1e-9, "dimension")
	assert.Equal("nomic-embed:768", active["fingerprint"], "fingerprint")
	assert.Equal("active", active["state"], "state")

	assert.NotContains(vs, "building_generation",
		"expected 'building_generation' to be absent when there is no building generation")
}

// inlineURL builds the inline endpoint URL for a given message ID and CID,
// URL-encoding the CID so values with reserved characters (including `/`)
// round-trip correctly.
func inlineURL(id int64, cid string) string {
	return fmt.Sprintf("/api/v1/messages/%d/inline?cid=%s", id, url.QueryEscape(cid))
}

// rawMIMEWithImagePart returns a multipart MIME message containing a single
// image part with the given Content-ID, content type, and Content-Disposition
// (typically "inline" or "attachment").
func rawMIMEWithImagePart(cid, contentType, disposition string, body []byte) []byte {
	boundary := "test-boundary-123"
	var b strings.Builder
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/related; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("Subject: test\r\n")
	b.WriteString("\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
	b.WriteString("<html><body><img src=\"cid:" + cid + "\"></body></html>\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: " + contentType + "\r\n")
	b.WriteString("Content-Disposition: " + disposition + "\r\n")
	b.WriteString("Content-ID: <" + cid + ">\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("\r\n")
	b.WriteString(base64.StdEncoding.EncodeToString(body))
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return []byte(b.String())
}

// rawMIMEWithInlineImage is a convenience wrapper for the common inline case.
func rawMIMEWithInlineImage(cid, contentType string, body []byte) []byte {
	return rawMIMEWithImagePart(cid, contentType, "inline", body)
}

func TestHandleMessageInline_ImagePNG(t *testing.T) {
	assert := assert.New(t)
	imgData := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	raw := rawMIMEWithInlineImage("logo@example", "image/png", imgData)

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, inlineURL(1, "logo@example"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal("image/png", w.Header().Get("Content-Type"), "Content-Type")
	cc := w.Header().Get("Cache-Control")
	assert.Contains(cc, "private", "Cache-Control should contain 'private'")
	assert.NotContains(cc, "public", "Cache-Control must not contain 'public'")
	assert.Equal("inline", w.Header().Get("Content-Disposition"), "Content-Disposition")
	assert.Equal(imgData, w.Body.Bytes(), "response body")
}

// TestHandleMessageInline_NonInlineSkipped ensures that a part with a matching
// Content-ID but Content-Disposition: attachment is not served via the inline
// endpoint — only parts flagged IsInline by the MIME parser should be reachable.
func TestHandleMessageInline_NonInlineSkipped(t *testing.T) {
	imgData := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	raw := rawMIMEWithImagePart("logo@example", "image/png", "attachment", imgData)

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, inlineURL(1, "logo@example"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "status for non-inline attachment")
}

func TestHandleMessageInline_RejectsXHTML(t *testing.T) {
	raw := rawMIMEWithInlineImage("evil@nasty", "application/xhtml+xml", []byte("<script>alert(1)</script>"))

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, inlineURL(1, "evil@nasty"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnsupportedMediaType, w.Code, "status for application/xhtml+xml inline part")
}

func TestHandleMessageInline_RejectsSVG(t *testing.T) {
	raw := rawMIMEWithInlineImage("vuln@svg", "image/svg+xml", []byte("<svg onload='alert(1)'/>"))

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, inlineURL(1, "vuln@svg"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnsupportedMediaType, w.Code, "status for image/svg+xml inline part")
}

func TestHandleMessageInline_CIDNotFound(t *testing.T) {
	raw := rawMIMEWithInlineImage("logo@example", "image/png", []byte{0x89, 'P', 'N', 'G'})

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, inlineURL(1, "nonexistent@cid"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "status")
}

func TestHandleMessageInline_NoEngine(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	req := httptest.NewRequest(http.MethodGet, inlineURL(1, "any@cid"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "status")
}

func TestHandleMessageInline_MessageNotFound(t *testing.T) {
	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, inlineURL(999, "any@cid"), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "status")
}

// TestHandleMessageInline_CIDWithSlash verifies that Content-IDs containing
// `/` round-trip correctly through the query parameter.
func TestHandleMessageInline_CIDWithSlash(t *testing.T) {
	cid := "path/with/slashes@example.com"
	imgData := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	raw := rawMIMEWithInlineImage(cid, "image/png", imgData)

	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: raw},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, inlineURL(1, cid), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal(t, imgData, w.Body.Bytes(), "response body")
}

// TestHandleMessageInline_MissingCID verifies that a request without the
// `cid` query parameter returns 400.
func TestHandleMessageInline_MissingCID(t *testing.T) {
	engine := &querytest.MockEngine{
		RawMessages: map[int64][]byte{1: rawMIMEWithInlineImage("logo@example", "image/png", []byte{0x89})},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/1/inline", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "status for missing cid")
}

// TestHandleMessageInline_UnsupportedEngine verifies that engines which
// can't fetch raw MIME (Postgres scaffold, remote engine) surface a stable
// 501 instead of a generic 500.
func TestHandleMessageInline_UnsupportedEngine(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"ErrNotImplemented", query.ErrNotImplemented},
		{"ErrNotSupported", daemonclient.ErrNotSupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := &querytest.MockEngine{
				GetMessageRawFunc: func(_ context.Context, _ int64) ([]byte, error) {
					return nil, tc.err
				},
			}
			srv := newTestServerWithEngine(t, engine)

			req := httptest.NewRequest(http.MethodGet, inlineURL(1, "logo@example"), nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			assert.Equal(t, http.StatusNotImplemented, w.Code, "status")
		})
	}
}

// TestHandleGetMessage_EngineUnsupportedFallsBackToStore verifies that when
// the configured engine reports the operation is unsupported, the handler
// falls through to the store path so engine-only errors don't break detail
// responses for engines that don't implement GetMessage.
func TestHandleGetMessage_EngineUnsupportedFallsBackToStore(t *testing.T) {
	engine := &querytest.MockEngine{
		GetMessageFunc: func(_ context.Context, _ int64) (*query.MessageDetail, error) {
			return nil, query.ErrNotImplemented
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/1", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.Equal(t, "Test Subject", resp["subject"], "subject (store path response)")
}

// newAttachmentTestServer builds a Server whose config points at a temp data
// dir, so AttachmentsDir() resolves to a writable location for seeding files.
func newAttachmentTestServer(t *testing.T, engine *querytest.MockEngine) (*Server, *config.Config) {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Data:   config.DataConfig{DataDir: t.TempDir()},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     &mockStore{},
		Engine:    engine,
		Scheduler: newMockScheduler(),
		Logger:    testLogger(),
	})
	return srv, cfg
}

// seedAttachmentFile writes content to the content-addressed path under the
// attachments dir (attachmentsDir/<hash[:2]>/<hash>).
func seedAttachmentFile(t *testing.T, cfg *config.Config, hash string, content []byte) {
	t.Helper()
	dir := filepath.Join(cfg.AttachmentsDir(), hash[:2])
	require.NoError(t, os.MkdirAll(dir, 0o755), "mkdir attachment dir")
	require.NoError(t, os.WriteFile(filepath.Join(dir, hash), content, 0o644), "write attachment file")
}

func TestHandleGetAttachmentContent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	hash := strings.Repeat("a1", 32) // 64 hex chars
	content := []byte("hello attachment world")

	engine := &querytest.MockEngine{
		AttachmentsByHash: map[string][]query.AttachmentInfo{
			hash: {{ID: 7, Filename: "report.pdf", MimeType: "application/pdf", Size: int64(len(content)), ContentHash: hash}},
		},
	}
	srv, cfg := newAttachmentTestServer(t, engine)
	seedAttachmentFile(t, cfg, hash, content)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/attachments/"+hash+"/content", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal("application/pdf", w.Header().Get("Content-Type"), "Content-Type")
	assert.Equal(`attachment; filename="report.pdf"`, w.Header().Get("Content-Disposition"), "Content-Disposition")
	assert.Equal(strconv.Itoa(len(content)), w.Header().Get("Content-Length"), "Content-Length")
	assert.Equal(content, w.Body.Bytes(), "body")
}

func TestHandleGetAttachmentContent_MissingMimeAndFilename(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	hash := strings.Repeat("bc", 32)
	content := []byte{0x00, 0x01, 0x02}

	engine := &querytest.MockEngine{
		AttachmentsByHash: map[string][]query.AttachmentInfo{
			hash: {{ID: 8, Size: int64(len(content)), ContentHash: hash}}, // no Filename, no MimeType
		},
	}
	srv, cfg := newAttachmentTestServer(t, engine)
	seedAttachmentFile(t, cfg, hash, content)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/attachments/"+hash+"/content", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal("application/octet-stream", w.Header().Get("Content-Type"), "Content-Type")
	assert.Equal("attachment", w.Header().Get("Content-Disposition"), "Content-Disposition")
}

func TestHandleGetAttachmentContent_InvalidHash(t *testing.T) {
	assert := assert.New(t)
	srv, _ := newAttachmentTestServer(t, &querytest.MockEngine{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/attachments/not-a-valid-hash/content", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusBadRequest, w.Code, "status (body: %s)", w.Body.String())
}

func TestHandleGetAttachmentContent_UnknownHash(t *testing.T) {
	assert := assert.New(t)
	// Valid hash shape, but no metadata row for it.
	srv, _ := newAttachmentTestServer(t, &querytest.MockEngine{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/attachments/"+strings.Repeat("de", 32)+"/content", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusNotFound, w.Code, "status (body: %s)", w.Body.String())
}

func TestHandleGetAttachmentContent_MetadataButFileMissing(t *testing.T) {
	assert := assert.New(t)
	hash := strings.Repeat("ef", 32)
	engine := &querytest.MockEngine{
		AttachmentsByHash: map[string][]query.AttachmentInfo{
			hash: {{ID: 9, Filename: "gone.bin", MimeType: "application/octet-stream", Size: 10, ContentHash: hash}},
		},
	}
	srv, _ := newAttachmentTestServer(t, engine) // note: no seedAttachmentFile

	req := httptest.NewRequest(http.MethodGet, "/api/v1/attachments/"+hash+"/content", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusNotFound, w.Code, "status (body: %s)", w.Body.String())
}

func TestResolveRecordedAttachmentPathRejectsUnsafePaths(t *testing.T) {
	assert := assert.New(t)
	attachmentsDir := t.TempDir()

	for _, storagePath := range []string{
		"../outside.bin",
		filepath.Join(string(filepath.Separator), "absolute.bin"),
		"https://example.com/attachment.bin",
	} {
		_, err := resolveRecordedAttachmentPath(attachmentsDir, storagePath)
		assert.Error(err, "storage path %q", storagePath)
	}
}

// TestHandleGetMessage_ExposesAttachmentContentHash verifies the message
// detail response carries each attachment's content_hash, so a client can
// discover the hash to pass to GET /api/v1/attachments/{hash}/content.
func TestHandleGetMessage_ExposesAttachmentContentHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	hash := strings.Repeat("ab", 32)
	engine := &querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{
			1: {
				ID:             1,
				Subject:        "With attachment",
				SentAt:         time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
				HasAttachments: true,
				Attachments: []query.AttachmentInfo{
					{ID: 5, Filename: "report.pdf", MimeType: "application/pdf", Size: 1234, ContentHash: hash},
				},
			},
		},
	}
	srv := newTestServerWithEngine(t, engine)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/1", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp struct {
		Attachments []map[string]any `json:"attachments"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	require.Len(resp.Attachments, 1, "attachments")
	assert.Equal(hash, resp.Attachments[0]["content_hash"], "content_hash")
}
