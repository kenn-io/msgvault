package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
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

func TestChatDiscoveryHTTPDeniesDelegatedAndMissingOwnerAuthentication(t *testing.T) {
	srv, grants := newTestServerWithAgentGrants(t)
	sources := []agentgrant.SourceRef{{ID: 1, Type: "beeper", Identifier: "synthetic-account"}}
	tokens := map[string]string{"missing": ""}
	for label, permissions := range map[string][]agentgrant.Permission{
		"draft": {agentgrant.PermissionDraftCreate},
		"read": {agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead,
			agentgrant.PermissionAttachmentRead, agentgrant.PermissionStatsRead},
	} {
		_, secret, _, err := grants.Issue("chat-"+label+"-test", permissions, sources, time.Time{})
		require.NoError(t, err)
		tokens[label] = secret
	}
	for label, token := range tokens {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/search?q=synthetic", nil)
		if token != "" {
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code, label)
	}
}
