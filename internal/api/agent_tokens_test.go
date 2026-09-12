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
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(err)

	// Hold the gate with a label so operationHealth() returns a labelled entry
	// and operationBusyHealth() returns Busy only.
	releaseGate, ok := gate.BeginLabeledWorkContext(context.Background(), "test-operation")
	require.True(ok, "must acquire gate")
	defer releaseGate()

	// Delegated caller.
	reqD := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	reqD.Header.Set(apiprotocol.AgentTokenHeader, secret)
	wD := httptest.NewRecorder()
	srv.Router().ServeHTTP(wD, reqD)
	require.Equal(http.StatusOK, wD.Code, "delegated health: %s", wD.Body.String())

	var delegatedResp HealthResponse
	require.NoError(json.NewDecoder(wD.Body).Decode(&delegatedResp))

	// Owner caller.
	reqO := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	reqO.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	wO := httptest.NewRecorder()
	srv.Router().ServeHTTP(wO, reqO)
	require.Equal(http.StatusOK, wO.Code, "owner health: %s", wO.Body.String())

	var ownerResp HealthResponse
	require.NoError(json.NewDecoder(wO.Body).Decode(&ownerResp))

	// Both report "ok" and carry APISchemaVersion.
	assert.Equal("ok", delegatedResp.Status)
	assert.Equal("ok", ownerResp.Status)
	assert.NotEmpty(delegatedResp.APISchemaVersion, "delegated health must include APISchemaVersion")
	assert.NotEmpty(ownerResp.APISchemaVersion, "owner health must include APISchemaVersion")
	assert.Equal(ownerResp.APISchemaVersion, delegatedResp.APISchemaVersion, "APISchemaVersion must match between delegated and owner")

	// Public projection: Operation.Busy is reported but Label is withheld.
	require.NotNil(delegatedResp.Operation, "delegated health must report operation busy when gate is held")
	assert.True(delegatedResp.Operation.Busy, "delegated operation must be busy")
	assert.Empty(delegatedResp.Operation.Label, "delegated operation must not expose label (public projection)")

	// Full projection: Operation.Label names the holder.
	require.NotNil(ownerResp.Operation, "owner health must report operation busy when gate is held")
	assert.Equal("test-operation", ownerResp.Operation.Label, "owner operation must expose label (full projection)")
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

func TestIssueAgentTokenUsesEffectiveRequestOrigin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	reg := agentgrant.NewRegistry()
	stub := &stubSourceStore{
		src: &store.Source{ID: 1, SourceType: "imap", Identifier: "alice@example.com"},
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIKey:         agentTokenTestAPIKey,
			AgentAccess:    true,
			TrustedProxies: []string{"127.0.0.1/32"},
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     stub,
		Logger:    testLogger(),
		Scheduler: newMockScheduler(),
	})
	srv.agentGrants = reg

	body, err := json.Marshal(agentTokenIssueRequest{
		Label:       "proxy-issued-agent",
		Permissions: []string{string(agentgrant.PermissionDraftCreate)},
		SourceIDs:   []int64{1},
	})
	require.NoError(err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	req.Header.Set("Forwarded", "for=192.0.2.20;proto=https;host=archive.example")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusCreated, w.Code, w.Body.String())
	var response agentTokenIssueResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&response))
	assert.Equal("https://archive.example", response.DaemonURL)
	assert.NotEmpty(response.Secret)
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
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newAgentTokenTestServer(t)

	reqBody := agentTokenIssueRequest{
		Label:       "secret-test",
		Permissions: []string{string(agentgrant.PermissionDraftCreate)},
		SourceIDs:   []int64{1},
	}
	bodyBytes, err := json.Marshal(reqBody)
	require.NoError(err)

	// Issue the token
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var issueResp agentTokenIssueResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&issueResp))
	require.NotEmpty(issueResp.Secret, "secret must be present in issue response")
	assert.Greater(len(issueResp.Secret), 10, "secret must be non-trivial")

	// List the tokens - secret must NOT appear
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/agent-tokens", nil)
	req2.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	w2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(w2, req2)
	require.Equal(http.StatusOK, w2.Code, "body: %s", w2.Body.String())

	var listResp agentTokenListResponse
	require.NoError(json.NewDecoder(w2.Body).Decode(&listResp))
	require.Len(listResp.Tokens, 1)
	for _, tok := range listResp.Tokens {
		assert.NotEmpty(tok.Label, "token view should have label")
		assert.NotEmpty(tok.ID, "token view should have ID")
	}
	// Verify no secret field in list body
	assert.NotContains(w2.Body.String(), issueResp.Secret, "secret must not appear in list response")
}

// TestAgentTokenRoutesExemptFromOperationGate proves the P1 fix: revoke and
// issue succeed with a 204/201 while the operation gate is held by another
// operation. This is the kill-switch invariant: a leaked agent token can be
// revoked even during a multi-hour sync or import, and the revoked grant is
// gone immediately.
func TestAgentTokenRoutesExemptFromOperationGate(t *testing.T) {
	require := require.New(t)
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

	// Issue a grant before holding the gate so we have an ID to revoke.
	prereq := agentTokenIssueRequest{
		Label:       "pre-hold",
		Permissions: []string{string(agentgrant.PermissionDraftCreate)},
		SourceIDs:   []int64{1},
	}
	prereqBytes, err := json.Marshal(prereq)
	require.NoError(err)
	preReq := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(prereqBytes))
	preReq.Header.Set("Content-Type", "application/json")
	preReq.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	preW := httptest.NewRecorder()
	srv.Router().ServeHTTP(preW, preReq)
	require.Equal(http.StatusCreated, preW.Code, "pre-hold issue: %s", preW.Body.String())
	var preIssued agentTokenIssueResponse
	require.NoError(json.NewDecoder(preW.Body).Decode(&preIssued))
	require.NotEmpty(preIssued.ID)

	// Hold the gate so all normal mutations would block.
	release, ok := gate.BeginLabeledWorkContext(context.Background(), "msgvault sync alice@example.com")
	require.True(ok, "must acquire gate")
	defer release()

	// Revoke must succeed (204) while gate is held.
	t.Run("revoke succeeds under gate contention", func(t *testing.T) {
		revokeReq := httptest.NewRequest(http.MethodDelete, "/api/v1/agent-tokens/"+preIssued.ID, nil)
		revokeReq.Header.Set("X-Api-Key", agentTokenTestAPIKey)
		revokeW := httptest.NewRecorder()
		srv.Router().ServeHTTP(revokeW, revokeReq)
		assert.Equal(t, http.StatusNoContent, revokeW.Code, "revoke while gate held: %s", revokeW.Body.String())

		// Grant must be gone immediately.
		grants := reg.List()
		for _, g := range grants {
			assert.NotEqual(t, preIssued.ID, g.ID, "revoked grant must not appear in registry")
		}
	})

	// Issue must also succeed (201) while gate is held.
	t.Run("issue succeeds under gate contention", func(t *testing.T) {
		issueReq := agentTokenIssueRequest{
			Label:       "under-contention",
			Permissions: []string{string(agentgrant.PermissionDraftCreate)},
			SourceIDs:   []int64{1},
		}
		issueBytes, marshalErr := json.Marshal(issueReq)
		require.NoError(marshalErr)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(issueBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", agentTokenTestAPIKey)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusCreated, w.Code, "issue while gate held: %s", w.Body.String())
	})
}

// TestHandleIssueAgentTokenValidation covers every validation branch added by
// this diff to handleIssueAgentToken. The registry enforces the same rules
// independently, but these tests pin the HTTP error contract (status + code)
// documented in docs/cli-reference.md.
func TestHandleIssueAgentTokenValidation(t *testing.T) {
	validBody := agentTokenIssueRequest{
		Label:       "test-agent",
		Permissions: []string{string(agentgrant.PermissionDraftCreate)},
		SourceIDs:   []int64{1},
	}

	cases := []struct {
		name       string
		body       any
		rawBody    string
		wantStatus int
		wantCode   string
		agentGrant bool // if false, server has agentGrants == nil
		srcErr     bool // if true, stubSourceStore returns an error
		noResolver bool // if true, use a store that does not implement agentGrantSourceResolver
	}{
		{
			name:       "agent_access_disabled",
			body:       validBody,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "agent_access_disabled",
			agentGrant: false,
		},
		{
			name:       "invalid_json_body",
			rawBody:    `{not valid json`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
			agentGrant: true,
		},
		{
			name: "label_required",
			body: agentTokenIssueRequest{
				Permissions: []string{string(agentgrant.PermissionDraftCreate)},
				SourceIDs:   []int64{1},
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
			agentGrant: true,
		},
		{
			name: "permissions_required",
			body: agentTokenIssueRequest{
				Label:     "test",
				SourceIDs: []int64{1},
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
			agentGrant: true,
		},
		{
			name: "source_ids_required",
			body: agentTokenIssueRequest{
				Label:       "test",
				Permissions: []string{string(agentgrant.PermissionDraftCreate)},
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
			agentGrant: true,
		},
		{
			name: "invalid_permission",
			body: agentTokenIssueRequest{
				Label:       "test",
				Permissions: []string{"unknown.operation"},
				SourceIDs:   []int64{1},
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_permission",
			agentGrant: true,
		},
		{
			name:       "store_does_not_support_source_resolution",
			body:       validBody,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
			agentGrant: true,
			noResolver: true,
		},
		{
			name:       "invalid_source",
			body:       validBody,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_source",
			agentGrant: true,
			srcErr:     true,
		},
		{
			name: "issue_error_remapped_to_400",
			// Two source IDs that both resolve to the same (Type, Identifier)
			// via the stub store trigger the registry's duplicate-source guard.
			body: agentTokenIssueRequest{
				Label:       "dup-source",
				Permissions: []string{string(agentgrant.PermissionDraftCreate)},
				SourceIDs:   []int64{1, 2},
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
			agentGrant: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var st MessageStore
			if tc.noResolver {
				// mockStore does not implement agentGrantSourceResolver.
				st = &mockStore{}
			} else {
				stub := &stubSourceStore{
					src: &store.Source{ID: 1, SourceType: "imap", Identifier: "alice@example.com"},
				}
				if tc.srcErr {
					stub.src = nil
					stub.srcErr = assert.AnError
				}
				st = stub
			}
			assert := assert.New(t)
			cfg := &config.Config{
				Server: config.ServerConfig{
					APIKey:      agentTokenTestAPIKey,
					AgentAccess: tc.agentGrant,
				},
			}
			srv := NewServerWithOptions(ServerOptions{
				Config:    cfg,
				Store:     st,
				Logger:    testLogger(),
				Scheduler: newMockScheduler(),
			})
			if tc.agentGrant {
				srv.agentGrants = agentgrant.NewRegistry()
			}

			var bodyBytes []byte
			if tc.rawBody != "" {
				bodyBytes = []byte(tc.rawBody)
			} else {
				var marshalErr error
				bodyBytes, marshalErr = json.Marshal(tc.body)
				require.NoError(t, marshalErr)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", agentTokenTestAPIKey)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			assert.Equal(tc.wantStatus, w.Code, "status: %s", w.Body.String())
			var errResp struct {
				Error string `json:"error"`
			}
			if assert.NoError(json.NewDecoder(w.Body).Decode(&errResp)) {
				assert.Equal(tc.wantCode, errResp.Error, "error code")
			}
		})
	}
}

// TestRevocationDeniesSubsequentAuthentication verifies the full HTTP
// issue → authenticate → revoke → authenticate cycle using only the HTTP
// endpoints (POST /api/v1/agent-tokens, GET /api/v1/health,
// DELETE /api/v1/agent-tokens/{id}).
func TestRevocationDeniesSubsequentAuthentication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newAgentTokenTestServer(t)

	// Step 1: issue a grant via HTTP.
	reqBody := agentTokenIssueRequest{
		Label:       "revoke-http-test",
		Permissions: []string{string(agentgrant.PermissionDraftCreate)},
		SourceIDs:   []int64{1},
	}
	bodyBytes, err := json.Marshal(reqBody)
	require.NoError(err)
	issueReq := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", bytes.NewReader(bodyBytes))
	issueReq.Header.Set("Content-Type", "application/json")
	issueReq.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	issueW := httptest.NewRecorder()
	srv.Router().ServeHTTP(issueW, issueReq)
	require.Equal(http.StatusCreated, issueW.Code, "issue: %s", issueW.Body.String())

	var issued agentTokenIssueResponse
	require.NoError(json.NewDecoder(issueW.Body).Decode(&issued))
	require.NotEmpty(issued.ID)
	require.NotEmpty(issued.Secret)

	// Step 2: prove the token authenticates.
	healthReq1 := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	healthReq1.Header.Set(apiprotocol.AgentTokenHeader, issued.Secret)
	healthW1 := httptest.NewRecorder()
	srv.Router().ServeHTTP(healthW1, healthReq1)
	assert.Equal(http.StatusOK, healthW1.Code, "pre-revocation: token must authenticate")

	// Step 3: revoke via HTTP.
	revokeReq := httptest.NewRequest(http.MethodDelete, "/api/v1/agent-tokens/"+issued.ID, nil)
	revokeReq.Header.Set("X-Api-Key", agentTokenTestAPIKey)
	revokeW := httptest.NewRecorder()
	srv.Router().ServeHTTP(revokeW, revokeReq)
	assert.Equal(http.StatusNoContent, revokeW.Code, "revoke: %s", revokeW.Body.String())

	// Step 4: prove the token no longer authenticates.
	healthReq2 := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	healthReq2.Header.Set(apiprotocol.AgentTokenHeader, issued.Secret)
	healthW2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(healthW2, healthReq2)
	assert.Equal(http.StatusUnauthorized, healthW2.Code, "post-revocation: revoked token must return 401")
}
