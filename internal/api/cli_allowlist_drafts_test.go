package api

import (
	"bytes"
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

func TestCLIRunDraftAllowlist(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"draft-reply", "42", "--from=alice@example.com", "--body=body"}))
	assert.False(t, cliRunCommandAllowed([]string{"configure-imap-drafts"}))
	assert.False(t, cliRunCommandAllowed([]string{"draft-reply"}))
}

// decodeRunErrorCode decodes the NDJSON error event code from a handleCLIRun response.
func decodeRunErrorCode(t *testing.T, body *bytes.Buffer) string {
	t.Helper()
	// The response body may be JSON or NDJSON. For errors returned early (before the stream),
	// we expect a plain JSON error response.
	var errResp ErrorResponse
	if err := json.NewDecoder(body).Decode(&errResp); err == nil && errResp.Error != "" {
		return errResp.Error
	}
	return ""
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
	t.Run("preservation invariant 13: Env→cliRunEnvAllowedForCommand, Cwd→runCLIReplyDraft:178", func(t *testing.T) {
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
