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
)

func TestCLIRunLineReplyOwnerAdmission(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	assert.True(cliRunCommandAllowed([]string{"send-line-reply", "chat-draft-example", "--revision=1"}))
	server, secret := newDelegatedTestServer(t)
	_, allSecret, _, err := server.agentGrants.Issue("reply-agent", []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "alice@example.com"}}, time.Time{})
	require.NoError(err)
	for _, token := range []string{secret, allSecret} {
		for _, args := range [][]string{{"send-line-reply", "missing-draft", "--revision=1"}, {"send-line-reply"}} {
			payload, err := json.Marshal(CLIRunRequest{Args: args})
			require.NoError(err)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(apiprotocol.AgentTokenHeader, token)
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			assert.Equal(http.StatusForbidden, response.Code)
			var result ErrorResponse
			require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
			assert.Equal("not_permitted", result.Error)
		}
	}
	payload, err := json.Marshal(CLIRunRequest{Args: []string{"send-line-reply", "missing-draft", "--revision=1"}, Env: map[string]string{"EXAMPLE_LINE_SECRET": "spoofed"}})
	require.NoError(err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "test-owner-key")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	assert.Equal(http.StatusBadRequest, response.Code)
	var result ErrorResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	assert.Equal("env_not_allowed", result.Error)
	assert.False(server.cliRunEnvAllowedForCommand([]string{"send-line-reply", "d1", "--revision=1"}, "EXAMPLE_LINE_SECRET"))
}
