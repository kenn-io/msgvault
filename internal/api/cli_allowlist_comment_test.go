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
)

func TestCommentModerateAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	assert.True(cliRunCommandAllowed([]string{"comment", "moderate", "--delete", "--yes", "42"}))
	assert.True(cliRunCommandAllowed([]string{"comment", "reply", "--body=Thanks", "42"}))
	assert.False(cliRunCommandAllowed([]string{"comment"}))
	assert.False(cliRunCommandAllowed([]string{"comment", "triage", "42"}))
}

func TestDelegatedCLIRunRefusesCommentModerate(t *testing.T) {
	t.Parallel()
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{
		agentgrant.PermissionVerificationRead, agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead,
		agentgrant.PermissionAttachmentRead, agentgrant.PermissionStatsRead, agentgrant.PermissionDraftCreate,
		agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete, agentgrant.PermissionCalendarRead,
		agentgrant.PermissionCalendarEventRead, agentgrant.PermissionCalendarWrite, agentgrant.PermissionCalendarInvite,
	}}
	for _, args := range [][]string{
		{"comment", "moderate", "42", "--delete", "--yes"},
		{"comment", "moderate", "42", "--hide"},
	} {
		t.Run(args[3], func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			require.True(cliRunCommandAllowed(args), "the owner may run it")
			runnerCalls := 0
			stub := &stubSourceStore{}
			stub.runFunc = func(context.Context, CLIRunRequest, func(CLIRunEvent) error) error {
				runnerCalls++
				return nil
			}
			srv := &Server{store: stub, logger: testLogger()}
			body, err := json.Marshal(CLIRunRequest{Args: args})
			require.NoError(err)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), requestSecurityContextKey{}, requestSecurity{
				auth: requestAuthentication{Mode: AuthModeDelegated, Grant: grant},
			}))
			resp := httptest.NewRecorder()
			srv.handleCLIRun(resp, req)
			assert.Equal(http.StatusBadRequest, resp.Code)
			assert.Zero(runnerCalls)
			var response ErrorResponse
			require.NoError(json.NewDecoder(resp.Body).Decode(&response))
			assert.Equal("command_not_allowed", response.Error)
		})
	}
}
