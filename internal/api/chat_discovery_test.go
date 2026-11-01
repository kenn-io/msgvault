package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestChatDiscoveryHTTPReadsRealArchive(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, id, _ := seedConversation(t, "beeper", 2)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/search?q=synthetic&limit=1", nil))
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var page store.ChatDiscoveryPage
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &page))
	requirements.Len(page.Results, 1)
	assertions.Equal(id, page.Results[0].ConversationID)
	assertions.Equal("no-store", w.Header().Get("Cache-Control"))
	for _, path := range []string{
		"/api/v1/conversations/search",
		"/api/v1/conversations/search?q=name&limit=0",
		"/api/v1/conversations/search?q=name&limit=bad",
		"/api/v1/conversations/search?q=name&source_id=0",
		"/api/v1/conversations/search?q=name&source_id=",
		"/api/v1/conversations/search?q=name&limit=",
	} {
		w = httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assertions.Equal(http.StatusBadRequest, w.Code, path)
	}
	for _, parameter := range []string{"source_id=%GG", "source_id=1;2", "limit=%GG", "limit=1;2"} {
		w = httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/search?q=synthetic&"+parameter, nil))
		assertions.Equal(http.StatusBadRequest, w.Code, parameter)
		var response ErrorResponse
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
		assertions.Equal("invalid_query", response.Error, parameter)
	}
}

func TestChatDiscoveryHTTPDeniesMissingOwnerAuthentication(t *testing.T) {
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "owner-key"}}, nil, nil, testLogger())
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/search?q=synthetic", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestChatDiscoveryHTTPDirtyMembershipCannotWriteDuringBackupFreeze(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })
	srv, conversationID, _ := seedConversation(t, "beeper", 1)
	st, ok := srv.store.(*store.Store)
	require.True(ok)
	srv.operationGate = NewSerialOperationGate()
	dirtyCount := func() int {
		var count int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM chat_members_dirty").Scan(&count))
		return count
	}
	require.Equal(1, dirtyCount())
	token := beginBackupFreeze(t, srv)
	t.Cleanup(func() {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, endBackupFreeze(t, token))
	})
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/search?q=synthetic", nil))
	require.Equal(http.StatusServiceUnavailable, w.Code, w.Body.String())
	var response ErrorResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal("operation_in_progress", response.Error)
	assert.Contains(response.Message, "backup freeze")
	assert.Equal(1, dirtyCount(), "freeze must leave membership writes queued")
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, endBackupFreeze(t, token))
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/search?q=synthetic", nil))
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	var page store.ChatDiscoveryPage
	require.NoError(json.Unmarshal(w.Body.Bytes(), &page))
	require.Len(page.Results, 1)
	assert.Equal(conversationID, page.Results[0].ConversationID)
	assert.Zero(dirtyCount(), "membership refresh resumes after freeze ends")
}

type chatDiscoveryScanStore struct {
	*store.Store

	gate *SerialOperationGate
	t    *testing.T
}

func (s *chatDiscoveryScanStore) SearchChatsMetadataContext(ctx context.Context, q store.ChatDiscoveryQuery) (*store.ChatDiscoveryPage, error) {
	_, _, held := s.gate.Holder()
	assert.False(s.t, held, "metadata scan must release the mutation gate")
	return s.Store.SearchChatsMetadataContext(ctx, q)
}

func TestChatDiscoveryHTTPReleasesGateBeforeMetadataScan(t *testing.T) {
	srv, _, _ := seedConversation(t, "beeper", 1)
	gate := NewSerialOperationGate()
	srv.operationGate = gate
	st, ok := srv.store.(*store.Store)
	require.True(t, ok)
	srv.store = &chatDiscoveryScanStore{Store: st, gate: gate, t: t}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/conversations/search?q=synthetic", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
