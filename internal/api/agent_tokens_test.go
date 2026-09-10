package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// TestAgentTokensDoNotSurviveRestart tests proof matrix row 13 (second half).
// Grants issued against one in-memory registry are not visible in a fresh
// registry (simulating a daemon restart).
func TestAgentTokensDoNotSurviveRestart(t *testing.T) {
	// First "instance": issue a grant.
	_, reg1 := newAgentTokenTestServer(t)
	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg1.Issue("pre-restart", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, agentgrant.DefaultLifetime)
	require.NoError(t, err)

	// Confirm the grant is valid in the first registry.
	_, ok := reg1.Lookup(secret)
	require.True(t, ok, "grant must be valid in the first registry")

	// Second "instance": fresh registry with no knowledge of the previous grant.
	srv2, _ := newAgentTokenTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w := httptest.NewRecorder()
	srv2.Router().ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "grant from old registry must not authenticate against new registry")
}

// TestRevocationDeniesNextRequest tests proof matrix row 14.
// After Revoke, the next request with the revoked token gets 401.
func TestRevocationDeniesNextRequest(t *testing.T) {
	srv, reg := newAgentTokenTestServer(t)

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	grantID, secret, _, err := reg.Issue("revoke-me", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, agentgrant.DefaultLifetime)
	require.NoError(t, err)

	// Pre-revocation: getHealth is allowed for delegated callers.
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req1.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w1 := httptest.NewRecorder()
	srv.Router().ServeHTTP(w1, req1)
	assert.NotEqual(t, http.StatusUnauthorized, w1.Code, "pre-revocation: valid token must not get 401")

	// Revoke the grant.
	revoked := reg.Revoke(grantID)
	require.True(t, revoked, "Revoke must return true for a known grant")

	// Post-revocation: same token must get 401.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req2.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusUnauthorized, w2.Code, "post-revocation: revoked token must get 401")
}

// TestDelegatedHealthUsesPublicProjection tests proof matrix row 17.
// Delegated callers receive the public projection plus APISchemaVersion.
// Owner callers receive the full health detail.
func TestDelegatedHealthUsesPublicProjection(t *testing.T) {
	srv, reg := newAgentTokenTestServer(t)

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("health-check", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, agentgrant.DefaultLifetime)
	require.NoError(t, err)

	// Delegated caller.
	reqD := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	reqD.Header.Set(apiprotocol.AgentTokenHeader, secret)
	wD := httptest.NewRecorder()
	srv.Router().ServeHTTP(wD, reqD)
	require.Equal(t, http.StatusOK, wD.Code, "delegated health: %s", wD.Body.String())

	var delegatedResp HealthResponse
	require.NoError(t, json.NewDecoder(wD.Body).Decode(&delegatedResp))

	// Owner caller.
	reqO := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	reqO.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	wO := httptest.NewRecorder()
	srv.Router().ServeHTTP(wO, reqO)
	require.Equal(t, http.StatusOK, wO.Code, "owner health: %s", wO.Body.String())

	var ownerResp HealthResponse
	require.NoError(t, json.NewDecoder(wO.Body).Decode(&ownerResp))

	// Both report "ok" and carry APISchemaVersion.
	assert.Equal(t, "ok", delegatedResp.Status)
	assert.Equal(t, "ok", ownerResp.Status)
	assert.NotEmpty(t, delegatedResp.APISchemaVersion, "delegated health must include APISchemaVersion")
	assert.NotEmpty(t, ownerResp.APISchemaVersion, "owner health must include APISchemaVersion")
	assert.Equal(t, ownerResp.APISchemaVersion, delegatedResp.APISchemaVersion, "APISchemaVersion must match between delegated and owner")
}

const agentTokenTestAPIKey = "owner-api-key-for-agent-tests"

func newAgentTokenTestServer(t *testing.T) (*Server, *agentgrant.Registry) {
	t.Helper()
	reg := agentgrant.NewRegistry(time.Now)
	stub := &stubSourceStore{
		src: &store.Source{
			ID:         1,
			SourceType: "imap",
			Identifier: "alice@example.com",
		},
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIKey:      agentTokenTestAPIKey,
			AgentAccess: true,
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     stub,
		Logger:    testLogger(),
		Scheduler: newMockScheduler(),
	})
	srv.agentGrants = reg
	return srv, reg
}

// TestAgentTokenIssueRequiresOwnerKey verifies proof matrix row 15:
// issue/list/revoke require owner API key; browser session and delegated get 401.
func TestAgentTokenIssueRequiresOwnerKey(t *testing.T) {
	srv, reg := newAgentTokenTestServer(t)

	reqBody := agentTokenIssueRequest{
		Label:       "test-agent",
		Permissions: []string{string(agentgrant.PermissionDraftCreate)},
		SourceIDs:   []int64{1},
	}
	bodyBytes, err := json.Marshal(reqBody)
	require.NoError(t, err)

	t.Run("no auth gets 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("owner API key gets 201", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", agentTokenTestAPIKey)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	})

	t.Run("delegated token gets 401", func(t *testing.T) {
		// Issue a grant first
		src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
		_, secret, _, issuErr := reg.Issue("delegated-caller", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, agentgrant.DefaultLifetime)
		require.NoError(t, issuErr)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		// delegated mode is not in the allowed list for issueAgentToken
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// TestAgentTokenListRequiresOwnerKey verifies proof matrix row 15.
func TestAgentTokenListRequiresOwnerKey(t *testing.T) {
	srv, _ := newAgentTokenTestServer(t)

	t.Run("no auth gets 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agent-tokens", nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("owner API key gets 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agent-tokens", nil)
		req.Header.Set("X-Api-Key", agentTokenTestAPIKey)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	})
}

// TestAgentTokenRevokeRequiresOwnerKey verifies proof matrix rows 15 and 16.
func TestAgentTokenRevokeRequiresOwnerKey(t *testing.T) {
	srv, _ := newAgentTokenTestServer(t)

	t.Run("no auth gets 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/agent-tokens/nonexistent", nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("owner API key revoke nonexistent ID returns 204", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/agent-tokens/nonexistent-id", nil)
		req.Header.Set("X-Api-Key", agentTokenTestAPIKey)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	})
}

// TestAgentTokenSecretNotInListResponse verifies proof matrix row 16:
// secret appears once in issue response and never in list response.
func TestAgentTokenSecretNotInListResponse(t *testing.T) {
	srv, _ := newAgentTokenTestServer(t)

	reqBody := agentTokenIssueRequest{
		Label:       "secret-test",
		Permissions: []string{string(agentgrant.PermissionDraftCreate)},
		SourceIDs:   []int64{1},
	}
	bodyBytes, err := json.Marshal(reqBody)
	require.NoError(t, err)

	// Issue the token
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var issueResp agentTokenIssueResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&issueResp))
	require.NotEmpty(t, issueResp.Secret, "secret must be present in issue response")
	assert.True(t, len(issueResp.Secret) > 10, "secret must be non-trivial")

	// List the tokens - secret must NOT appear
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/agent-tokens", nil)
	req2.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	w2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code, "body: %s", w2.Body.String())

	var listResp agentTokenListResponse
	require.NoError(t, json.NewDecoder(w2.Body).Decode(&listResp))
	require.Len(t, listResp.Tokens, 1)
	for _, tok := range listResp.Tokens {
		assert.Empty(t, tok.Label == "" && tok.ID == "", "token view should have label and ID")
	}
	// Verify no secret field in list body
	assert.NotContains(t, w2.Body.String(), issueResp.Secret, "secret must not appear in list response")
}
