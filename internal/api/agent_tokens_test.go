package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
	_, secret, _, err := reg1.Issue("pre-restart", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
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

// TestDelegatedHealthUsesPublicProjection tests proof matrix row 17.
// Delegated callers receive the public projection (operationBusyHealth) plus
// APISchemaVersion. Owner callers receive the full projection (operationHealth)
// which includes the operation label. When the gate is held, the two bodies
// differ on Operation.Label: delegated sees none, owner sees the label.
func TestDelegatedHealthUsesPublicProjection(t *testing.T) {
	gate := NewSerialOperationGate()
	stub := &stubSourceStore{
		src: &store.Source{ID: 1, SourceType: "imap", Identifier: "alice@example.com"},
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIKey:      agentTokenTestAPIKey,
			AgentAccess: true,
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:        cfg,
		Store:         stub,
		Logger:        testLogger(),
		Scheduler:     newMockScheduler(),
		OperationGate: gate,
	})
	reg := agentgrant.NewRegistry()
	srv.agentGrants = reg

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("health-check", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
	require.NoError(t, err)

	// Hold the gate with a label so operationHealth() returns a labelled entry
	// and operationBusyHealth() returns Busy only.
	releaseGate, ok := gate.BeginLabeledWorkContext(context.Background(), "test-operation")
	require.True(t, ok, "must acquire gate")
	defer releaseGate()

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

	// Public projection: Operation.Busy is reported but Label is withheld.
	require.NotNil(t, delegatedResp.Operation, "delegated health must report operation busy when gate is held")
	assert.True(t, delegatedResp.Operation.Busy, "delegated operation must be busy")
	assert.Empty(t, delegatedResp.Operation.Label, "delegated operation must not expose label (public projection)")

	// Full projection: Operation.Label names the holder.
	require.NotNil(t, ownerResp.Operation, "owner health must report operation busy when gate is held")
	assert.Equal(t, "test-operation", ownerResp.Operation.Label, "owner operation must expose label (full projection)")
}

const agentTokenTestAPIKey = "owner-api-key-for-agent-tests"

func newAgentTokenTestServer(t *testing.T) (*Server, *agentgrant.Registry) {
	t.Helper()
	reg := agentgrant.NewRegistry()
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
		_, secret, _, issuErr := reg.Issue("delegated-caller", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
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
		assert.NotEmpty(t, tok.Label, "token view should have label")
		assert.NotEmpty(t, tok.ID, "token view should have ID")
	}
	// Verify no secret field in list body
	assert.NotContains(t, w2.Body.String(), issueResp.Secret, "secret must not appear in list response")
}

// TestRevocationDeniesSubsequentAuthentication verifies the full HTTP
// issue → authenticate → revoke → authenticate cycle using only the HTTP
// endpoints (POST /api/v1/agent-tokens, GET /api/v1/health,
// DELETE /api/v1/agent-tokens/{id}).
func TestRevocationDeniesSubsequentAuthentication(t *testing.T) {
	srv, _ := newAgentTokenTestServer(t)

	// Step 1: issue a grant via HTTP.
	reqBody := agentTokenIssueRequest{
		Label:       "revoke-http-test",
		Permissions: []string{string(agentgrant.PermissionDraftCreate)},
		SourceIDs:   []int64{1},
	}
	bodyBytes, err := json.Marshal(reqBody)
	require.NoError(t, err)
	issueReq := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(bodyBytes))
	issueReq.Header.Set("Content-Type", "application/json")
	issueReq.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	issueW := httptest.NewRecorder()
	srv.Router().ServeHTTP(issueW, issueReq)
	require.Equal(t, http.StatusCreated, issueW.Code, "issue: %s", issueW.Body.String())

	var issued agentTokenIssueResponse
	require.NoError(t, json.NewDecoder(issueW.Body).Decode(&issued))
	require.NotEmpty(t, issued.ID)
	require.NotEmpty(t, issued.Secret)

	// Step 2: prove the token authenticates.
	healthReq1 := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	healthReq1.Header.Set(apiprotocol.AgentTokenHeader, issued.Secret)
	healthW1 := httptest.NewRecorder()
	srv.Router().ServeHTTP(healthW1, healthReq1)
	assert.Equal(t, http.StatusOK, healthW1.Code, "pre-revocation: token must authenticate")

	// Step 3: revoke via HTTP.
	revokeReq := httptest.NewRequest(http.MethodDelete, "/api/v1/agent-tokens/"+issued.ID, nil)
	revokeReq.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	revokeW := httptest.NewRecorder()
	srv.Router().ServeHTTP(revokeW, revokeReq)
	assert.Equal(t, http.StatusNoContent, revokeW.Code, "revoke: %s", revokeW.Body.String())

	// Step 4: prove the token no longer authenticates.
	healthReq2 := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	healthReq2.Header.Set(apiprotocol.AgentTokenHeader, issued.Secret)
	healthW2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(healthW2, healthReq2)
	assert.Equal(t, http.StatusUnauthorized, healthW2.Code, "post-revocation: revoked token must return 401")
}
