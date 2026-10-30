package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
)

func TestThreadsOAuthHandoffCommandRestriction(t *testing.T) {
	assert := assert.New(t)
	srv := &Server{}
	assert.True(clirun.EnvAllowed(clirun.EnvThreadsOAuth))
	assert.True(cliRunCommandAllowed([]string{"add-threads"}))
	assert.True(srv.cliRunEnvAllowedForCommand([]string{"add-threads"}, clirun.EnvThreadsOAuth))
	for _, command := range []string{"sync", "sync-threads", "add-inline", "export-messages", "draft-compose"} {
		assert.False(srv.cliRunEnvAllowedForCommand([]string{command}, clirun.EnvThreadsOAuth), command)
	}
	assert.False(srv.cliRunEnvAllowedForCommand(nil, clirun.EnvThreadsOAuth))
}

func TestThreadsOAuthDelegatedHTTPDeniedBeforeRunner(t *testing.T) {
	assert := assert.New(t)
	calls := 0
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}},
		Store:  &mockStore{runFunc: func(context.Context, CLIRunRequest, func(CLIRunEvent) error) error { calls++; return nil }},
		Logger: testLogger(),
	})
	t.Cleanup(srv.agentGrants.Close)
	_, token, _, err := srv.agentGrants.Issue("draft creator", []agentgrant.Permission{agentgrant.PermissionDraftCreate},
		[]agentgrant.SourceRef{{ID: 1, Type: "threads", Identifier: "threads:123:20"}}, time.Time{})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(`{"args":["add-threads"],"env":{"MSGVAULT_THREADS_OAUTH":"synthetic-payload"}}`))
	request.Header.Set(apiprotocol.AgentTokenHeader, token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	srv.Router().ServeHTTP(recorder, request)
	assert.Equal(http.StatusBadRequest, recorder.Code)
	assert.Contains(recorder.Body.String(), "command_not_allowed")
	assert.Zero(calls)
}

func TestThreadsSyncCLIAndScheduleAdmission(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"sync-threads", "threads:123:20"}))
	assert.Equal(t, sourceScheduleAccount, classifySourceScheduling("threads", "threads:123:20").kind)
}
