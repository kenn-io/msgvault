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

func TestBeeperDraftPermissionMapping(t *testing.T) {
	assertions := assert.New(t)
	cases := []struct {
		args []string
		want agentgrant.Permission
	}{
		{[]string{"draft-beeper", "create"}, agentgrant.PermissionDraftCreate},
		{[]string{"draft-beeper", "get"}, agentgrant.PermissionDraftRead},
		{[]string{"draft-beeper", "edit"}, agentgrant.PermissionDraftEdit},
		{[]string{"draft-beeper", "clear"}, agentgrant.PermissionDraftDelete},
	}
	for _, tc := range cases {
		got, ok := BeeperDraftPermission(tc.args)
		assertions.True(ok)
		assertions.Equal(tc.want, got)
		assertions.True(IsCLIRunBeeperDraft(tc.args))
		assertions.True(cliRunCommandAllowed(tc.args))
	}
	assertions.False(IsCLIRunBeeperDraft([]string{"draft-beeper"}))
	assertions.True(IsCLIRunBeeperDraft([]string{"draft-beeper", "get", "other"}))
	assertions.False(IsCLIRunBeeperDraft([]string{"get", "draft-id"}))
}

func TestBeeperDraftDelegatedAdmissionUsesReadPermission(t *testing.T) {
	assertions := assert.New(t)
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftRead}}
	assertions.True(delegatedCLIRunAdmitted([]string{"draft-beeper", "get", "beeper-draft"}, grant))
	assertions.False(delegatedCLIRunAdmitted([]string{"draft-beeper", "create"}, grant))
	assertions.False(delegatedCLIRunAdmitted([]string{"draft-beeper", "get", "beeper-draft"}, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}))
}

func TestBeeperDraftAdmission(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	registry := agentgrant.NewRegistry()
	source := &store.Source{ID: 42, SourceType: "beeper", Identifier: "signal"}
	ref := agentgrant.SourceRef{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}
	_, readSecret, _, err := registry.Issue("beeper-read", []agentgrant.Permission{agentgrant.PermissionDraftRead}, []agentgrant.SourceRef{ref})
	requirements.NoError(err)
	_, createSecret, _, err := registry.Issue("beeper-create", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{ref})
	requirements.NoError(err)
	runnerCalls := 0
	stub := &stubSourceStore{src: source}
	stub.runFunc = func(_ context.Context, req CLIRunRequest, _ func(CLIRunEvent) error) error {
		runnerCalls++
		assertions.Equal([]string{"draft-beeper", "get", "beeper-draft"}, req.Args)
		return nil
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:    &config.Config{Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}},
		Store:     stub,
		Logger:    testLogger(),
		Scheduler: newMockScheduler(),
	})
	srv.agentGrants = registry
	send := func(secret string, request CLIRunRequest) *httptest.ResponseRecorder {
		body, marshalErr := json.Marshal(request)
		requirements.NoError(marshalErr)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		return response
	}

	response := send(readSecret, CLIRunRequest{Args: []string{"draft-beeper", "get", "beeper-draft"}})
	assertions.Equal(http.StatusOK, response.Code)
	assertions.Equal(1, runnerCalls)

	response = send(createSecret, CLIRunRequest{Args: []string{"draft-beeper", "get", "beeper-draft"}})
	assertions.Equal(http.StatusBadRequest, response.Code)
	assertions.Equal(1, runnerCalls)
	assertions.Contains(response.Body.String(), "command_not_allowed")

	response = send(readSecret, CLIRunRequest{Args: []string{"draft-beeper", "get", "beeper-draft"}, Env: map[string]string{"HOME": "blocked"}})
	assertions.Equal(http.StatusBadRequest, response.Code)
	assertions.Equal(1, runnerCalls)
	assertions.Contains(response.Body.String(), "env_not_allowed")
}
