package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func TestCLIRunDraftAllowlist(t *testing.T) {
	assertions := assert.New(t)
	assertions.True(cliRunCommandAllowed([]string{"draft-reply", "42", "--from=alice@example.com", "--body=body"}))
	assertions.True(IsCLIRunDraftLifecycle([]string{"draft-get", "draft-abc"}))
	assertions.True(cliRunCommandAllowed([]string{"draft-edit", "draft-abc", "--revision=1", "--body=body"}))
	assertions.True(cliRunCommandAllowed([]string{"draft-delete", "draft-abc", "--revision=1"}))
	assertions.True(IsCLIRunDraftLifecycle([]string{"draft-recover", "draft-abc", "--revision=1"}))
	assertions.True(cliRunCommandAllowed([]string{"draft-recover", "draft-abc", "--revision=1"}))
	assertions.False(cliRunCommandAllowed([]string{"configure-imap-drafts"}))
	assertions.False(cliRunCommandAllowed([]string{"draft-reply"}))
	assertions.False(cliRunCommandAllowed([]string{"draft-get"}))
}

func TestDelegatedDraftRecoverRequiresActionPermission(t *testing.T) {
	source := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	assert.True(t, delegatedCLIRunAdmitted(
		[]string{"draft-recover", "draft-abc", "--revision=1"},
		&agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftEdit}},
	))
	assert.True(t, delegatedCLIRunAdmitted(
		[]string{"draft-recover", "draft-abc", "--revision=1"},
		&agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftDelete}},
	))
	assert.False(t, delegatedCLIRunAdmitted(
		[]string{"draft-recover", "draft-abc", "--revision=1"},
		&agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{source}},
	))
}

// newDelegatedTestServer creates a server with agentGrants enabled and issues a grant.
func newDelegatedTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	return newDelegatedTestServerWithGate(t, nil)
}

func newDelegatedTestServerWithGate(t *testing.T, gate OperationGate) (*Server, string) {
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
		Config:        cfg,
		Store:         stub,
		Logger:        testLogger(),
		Scheduler:     newMockScheduler(),
		OperationGate: gate,
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
		bs, marshalErr := json.Marshal(body)
		require.NoError(t, marshalErr)
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
		bs, marshalErr := json.Marshal(CLIRunRequest{Args: []string{"add-imap", "imap://example.com"}})
		require.NoError(t, marshalErr)
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
		bs, marshalErr := json.Marshal(CLIRunRequest{Args: []string{"remove-account"}})
		require.NoError(t, marshalErr)
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
		bs, marshalErr := json.Marshal(CLIRunRequest{Args: []string{"query", "SELECT 1"}})
		require.NoError(t, marshalErr)
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
		envBody, marshalErr := json.Marshal(CLIRunRequest{Args: []string{"draft-reply", "42"}, Env: map[string]string{"X": "y"}})
		require.NoError(t, marshalErr)
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

func TestDelegatedCLIRunRequiresGrantedPermission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grant *agentgrant.Grant
		code  int
		calls int
	}{
		{name: "draft.create", grant: &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}, code: http.StatusOK, calls: 1},
		{name: "missing permission", grant: &agentgrant.Grant{}, code: http.StatusBadRequest},
		{name: "nil grant", code: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			runnerCalls := 0
			stub := &stubSourceStore{}
			stub.runFunc = func(context.Context, CLIRunRequest, func(CLIRunEvent) error) error {
				runnerCalls++
				return nil
			}
			srv := &Server{store: stub, logger: testLogger()}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewBufferString(`{"args":["draft-reply","42"]}`))
			req = req.WithContext(context.WithValue(req.Context(), requestSecurityContextKey{}, requestSecurity{
				auth: requestAuthentication{Mode: AuthModeDelegated, Grant: tc.grant},
			}))
			resp := httptest.NewRecorder()
			srv.handleCLIRun(resp, req)
			assert.Equal(tc.code, resp.Code)
			assert.Equal(tc.calls, runnerCalls)
			if tc.code != http.StatusOK {
				var response ErrorResponse
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&response))
				assert.Equal("command_not_allowed", response.Error)
			}
		})
	}
}

func TestDelegatedDraftLifecycleCommandsSkipBusyOperationGate(t *testing.T) {
	gate := NewSerialOperationGate()
	srv, secret := newDelegatedTestServerWithGate(t, gate)
	serverStore, ok := srv.store.(*stubSourceStore)
	setupRequirements := require.New(t)
	setupRequirements.True(ok, "delegated fixture must expose its stub store")

	runnerCalls := 0
	serverStore.runFunc = func(_ context.Context, _ CLIRunRequest, _ func(CLIRunEvent) error) error {
		runnerCalls++
		return nil
	}

	oldWaitLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldWaitLimit })

	release, ok := gate.BeginLabeledWorkContext(context.Background(), "owner draft operation")
	setupRequirements.True(ok, "hold operation gate for delegated requests")
	defer release()

	commands := []struct {
		name string
		args []string
	}{
		{name: "get", args: []string{"draft-get", "draft-abc"}},
		{name: "edit", args: []string{"draft-edit", "draft-abc", "--revision=1", "--body=updated"}},
		{name: "delete", args: []string{"draft-delete", "draft-abc", "--revision=1"}},
	}

	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			body, err := json.Marshal(CLIRunRequest{Args: command.args})
			requirements.NoError(err)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(apiprotocol.AgentTokenHeader, secret)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				srv.Router().ServeHTTP(response, req)
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(200 * time.Millisecond):
				requirements.FailNow("delegated lifecycle rejection must not wait on a held operation gate")
			}

			assertions.Equal(http.StatusBadRequest, response.Code)
			var apiError ErrorResponse
			requirements.NoError(json.NewDecoder(response.Body).Decode(&apiError))
			assertions.Equal("command_not_allowed", apiError.Error)
			assertions.Equal(0, runnerCalls)
			assertions.False(gate.HasRequestWaiters())
		})
	}
}

func TestOwnerDraftLifecycleCommandsUseOperationGateAndRunner(t *testing.T) {
	gate := &recordingOperationGate{allow: true}
	srv, _ := newDelegatedTestServerWithGate(t, gate)
	serverStore, ok := srv.store.(*stubSourceStore)
	setupRequirements := require.New(t)
	setupRequirements.True(ok, "owner fixture must expose its stub store")

	runnerCalls := 0
	serverStore.runFunc = func(_ context.Context, _ CLIRunRequest, _ func(CLIRunEvent) error) error {
		runnerCalls++
		return nil
	}

	commands := []struct {
		name string
		args []string
	}{
		{name: "get", args: []string{"draft-get", "draft-abc"}},
		{name: "edit", args: []string{"draft-edit", "draft-abc", "--revision=1", "--body=updated"}},
		{name: "delete", args: []string{"draft-delete", "draft-abc", "--revision=1"}},
	}

	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			body, err := json.Marshal(CLIRunRequest{Args: command.args})
			requirements.NoError(err)
			beforeRuns := runnerCalls
			beforeBegins, beforeDone := gate.counts()

			req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", "test-owner-key")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, req)

			assertions.Equal(http.StatusOK, response.Code)
			assertions.Equal(beforeRuns+1, runnerCalls)
			begins, done := gate.counts()
			if command.name == "get" {
				assertions.Equal(beforeBegins, begins)
				assertions.Equal(beforeDone, done)
			} else {
				assertions.Equal(beforeBegins+1, begins)
				assertions.Equal(beforeDone+1, done)
			}
		})
	}
}

// TestDelegatedGrantScopesSource is the mutation probe for cli_handlers.go:1315.
// It drives a delegated draft-reply through the real handler against a source
// that is not in the grant, and asserts the request is refused.
// Deleting the line `req.Grant = auth.Grant` causes the runFunc to receive a nil
// grant, which bypasses the source check and produces a 200 with no error event,
// making the assertion fail.
func TestDelegatedGrantScopesSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	reg := agentgrant.NewRegistry()
	grantedSrc := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("scope-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{grantedSrc})
	require.NoError(err)

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
			return &CLIRunCodedError{Code: "not_permitted", Err: errors.New("source not in grant")}
		}
		return nil
	}

	body, marshalErr := json.Marshal(CLIRunRequest{Args: []string{"draft-reply", "42"}})
	require.NoError(marshalErr)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code)
	assert.Contains(w.Body.String(), "not_permitted",
		"delegated draft-reply for out-of-grant source must be refused; if this fails, verify cli_handlers.go:1315 sets req.Grant = auth.Grant")
}
