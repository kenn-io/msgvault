package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestCLIRunDraftAllowlist(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"draft-reply", "42", "--from=alice@example.com", "--body=body"}))
	assert.False(t, cliRunCommandAllowed([]string{"configure-imap-drafts"}))
	assert.False(t, cliRunCommandAllowed([]string{"draft-reply"}))
}

// newDelegatedTestServer creates a server with agentGrants enabled and issues a grant.
func newDelegatedTestServer(t *testing.T) (*Server, string) {
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
			APIKey:      "test-owner-key",
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

	// Issue a grant and return the secret
	srcRef := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("test-agent", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{srcRef})
	require.NoError(t, err)

	return srv, secret
}

// TestDelegatedCLIRunAdmission tests proof matrix row 9.
func TestDelegatedCLIRunAdmission(t *testing.T) {
	srv, secret := newDelegatedTestServer(t)

	// Helper: send a run request as delegated caller with the full router
	sendDelegated := func(args []string, extraHeaders ...func(*http.Request)) int {
		body := CLIRunRequest{Args: args}
		bs, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(bs))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		for _, h := range extraHeaders {
			h(req)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w.Code
	}

	t.Run("draft-reply with 2 args admitted (200 from runner)", func(t *testing.T) {
		// draft-reply with ≥2 args is admitted — runner returns nil (mockStore.RunCLICommand)
		code := sendDelegated([]string{"draft-reply", "42", "--from=alice@example.com", "--body=hi"})
		// 200 means it reached the runner (no early rejection)
		assert.Equal(t, http.StatusOK, code)
	})

	t.Run("add-imap returns command_not_allowed", func(t *testing.T) {
		bs, _ := json.Marshal(CLIRunRequest{Args: []string{"add-imap", "imap://example.com"}})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(bs))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		// add-imap is not in cliRunCommandAllowed at all → command_not_allowed
		var resp ErrorResponse
		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		assert.Equal(t, "command_not_allowed", resp.Error)
	})

	t.Run("remove-account returns command_not_allowed", func(t *testing.T) {
		bs, _ := json.Marshal(CLIRunRequest{Args: []string{"remove-account"}})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(bs))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		var resp ErrorResponse
		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		assert.Equal(t, "command_not_allowed", resp.Error)
	})

	t.Run("query returns command_not_allowed for delegated", func(t *testing.T) {
		// query is allowlisted for owner but not for delegated
		bs, _ := json.Marshal(CLIRunRequest{Args: []string{"query", "SELECT 1"}})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(bs))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		var resp ErrorResponse
		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		assert.Equal(t, "command_not_allowed", resp.Error)
	})

	// PI-13: a delegated draft-reply carrying non-empty Env or Cwd is rejected
	// before the operation starts. Two separate owners cover the gates after
	// the handleCLIRun delegated arm was trimmed to its two essential statements.
	t.Run("preservation invariant 13: Env→cliRunEnvAllowedForCommand", func(t *testing.T) {
		// Env: cliRunEnvAllowedForCommand returns false for every env name on
		// draft-reply, so handleCLIRun writes env_not_allowed (HTTP 400) before
		// calling the runner.
		envBody, _ := json.Marshal(CLIRunRequest{Args: []string{"draft-reply", "42"}, Env: map[string]string{"X": "y"}})
		envReq := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(envBody))
		envReq.Header.Set("Content-Type", "application/json")
		envReq.Header.Set(apiprotocol.AgentTokenHeader, secret)
		envW := httptest.NewRecorder()
		srv.Router().ServeHTTP(envW, envReq)
		var envResp ErrorResponse
		require.NoError(t, json.NewDecoder(envW.Body).Decode(&envResp))
		assert.Equal(t, "env_not_allowed", envResp.Error) // cliRunEnvAllowedForCommand

	})
}

// TestDelegatedGrantScopesSource is the mutation probe for cli_handlers.go:1315.
// It drives a delegated draft-reply through the real handler against a source
// that is not in the grant, and asserts the request is refused.
// Deleting the line `req.Grant = auth.Grant` causes the runFunc to receive a nil
// grant, which bypasses the source check and produces a 200 with no error event,
// making the assertion fail.
func TestDelegatedGrantScopesSource(t *testing.T) {
	reg := agentgrant.NewRegistry()
	grantedSrc := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("scope-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{grantedSrc})
	require.NoError(t, err)

	// The store resolves the parent message to a source outside the grant.
	outOfGrant := &store.Source{ID: 2, SourceType: "imap", Identifier: "bob@example.com"}
	stub := &stubSourceStore{src: outOfGrant}
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: stub, Logger: testLogger(), Scheduler: newMockScheduler()})
	srv.agentGrants = reg

	// runFunc simulates authorizeDelegatedDraftSource: a nil grant passes through
	// (owner path), a non-nil grant that does not cover the source returns not_permitted.
	stub.runFunc = func(_ context.Context, req CLIRunRequest, _ func(CLIRunEvent) error) error {
		if req.Grant == nil {
			return nil
		}
		ref := agentgrant.SourceRef{ID: outOfGrant.ID, Type: outOfGrant.SourceType, Identifier: outOfGrant.Identifier}
		if !req.Grant.Allows(agentgrant.PermissionDraftCreate, ref) {
			return &CLIRunCodedError{Code: "not_permitted", Err: fmt.Errorf("source not in grant")}
		}
		return nil
	}

	body, _ := json.Marshal(CLIRunRequest{Args: []string{"draft-reply", "42"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "not_permitted",
		"delegated draft-reply for out-of-grant source must be refused; if this fails, verify cli_handlers.go:1315 sets req.Grant = auth.Grant")
}
