package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func (s *inboxCandidateHTTPStore) InboxContext(ctx context.Context, request inboxcontrol.ContextRequest) (*inboxcontrol.Context, error) {
	return s.archive.InboxContext(ctx, request)
}

// Uses the native router, issued grants and Store lookup. Removing the separate
// content permission would let a metadata-only reader see the message body.
func TestInboxContextHTTPExactSourceContentAuthorization(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	mid := f.CreateMessage("context-http")
	target := inboxcontrol.Target{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "context-http"}
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), mid, "Synthetic message body")
	requirements.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, &inboxCandidateHTTPStore{archive: f.Store}, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	issue := func(permissions ...agentgrant.Permission) (string, string) {
		t.Helper()
		id, secret, _, err := srv.agentGrants.Issue("context-reader", permissions, []agentgrant.SourceRef{{ID: target.SourceID, Type: target.SourceType, Identifier: target.SourceIdentifier}})
		require.NoError(t, err)
		return id, secret
	}
	call := func(body, owner, agent string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/inbox/context", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if owner != "" {
			r.Header.Set("Authorization", "Bearer "+owner)
		}
		if agent != "" {
			r.Header.Set(apiprotocol.AgentTokenHeader, agent)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	request := inboxcontrol.ContextRequest{Target: target, MaxBytes: 9}
	encoded, err := json.Marshal(request)
	requirements.NoError(err)
	_, metadata := issue(agentgrant.PermissionInboxRead)
	response := call(string(encoded), "", metadata)
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	assertions.NotContains(response.Body.String(), "Synthetic")
	_, contentOnly := issue(agentgrant.PermissionInboxContentRead)
	response = call(string(encoded), "", contentOnly)
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	grantID, reader := issue(agentgrant.PermissionInboxRead, agentgrant.PermissionInboxContentRead)
	response = call(string(encoded), "", reader)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal("no-store", response.Header().Get("Cache-Control"))
	var result inboxcontrol.Context
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	assertions.Equal("Synthetic", result.Text)
	assertions.True(result.Truncated)
	assertions.Equal(target, result.Target)
	assertions.Equal(mid, result.MessageID)
	foreign := request
	foreign.Target.AccountID = "foreign@example.test"
	body, err := json.Marshal(foreign)
	requirements.NoError(err)
	response = call(string(body), "synthetic-owner-key", "")
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	foreign = request
	foreign.Target.SourceIdentifier = "foreign@example.test"
	body, err = json.Marshal(foreign)
	requirements.NoError(err)
	response = call(string(body), "", reader)
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	for _, bad := range []string{string(encoded) + " {}", strings.Replace(string(encoded), `"max_bytes":9`, `"max_bytes":65537`, 1), string(bytes.TrimSuffix(encoded, []byte("}"))) + `,"unknown":true}`} {
		response = call(bad, "synthetic-owner-key", "")
		assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	}
	for _, secret := range []string{metadata, contentOnly, reader} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(http.StatusOK, w.Code)
		var descriptor apiprotocol.MCPCapabilities
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &descriptor))
		found := false
		for _, route := range descriptor.Routes {
			if route.OperationID == "getInboxContext" {
				found = true
				assertions.Equal(http.MethodPost, route.Method)
				assertions.Equal("/api/v1/inbox/context", route.Path)
				assertions.ElementsMatch([]string{"target", "message_id", "max_bytes"}, route.RequestProperties)
			}
		}
		assertions.Equal(secret == reader, found)
	}
	response = call(string(encoded), "", "")
	assertions.Equal(http.StatusUnauthorized, response.Code, response.Body.String())
	requirements.True(srv.agentGrants.Revoke(grantID))
	response = call(string(encoded), "", reader)
	assertions.Equal(http.StatusUnauthorized, response.Code, response.Body.String())
}
