package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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

func TestDelegatedCLIRunRefusesCommentActions(t *testing.T) {
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
		{"comment", "reply", "42", "--body", "Thanks"},
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

func TestRedactedCLIRunArgs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		args, want []string
	}{
		{"separate value", []string{"comment", "reply", "42", "--body", "Thanks"}, []string{"comment", "reply", "42", "--body", "[redacted]"}},
		{"joined value", []string{"comment", "reply", "--body=Thanks", "42"}, []string{"comment", "reply", "--body=[redacted]", "42"}},
		{"missing value", []string{"comment", "reply", "42", "--body"}, []string{"comment", "reply", "42", "--body"}},
		{"other command", []string{"comment", "moderate", "42", "--body", "Thanks"}, []string{"comment", "moderate", "42", "--body", "Thanks"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := slices.Clone(tt.args)
			assert.Equal(t, tt.want, RedactedCLIRunArgs(tt.args))
			assert.Equal(t, original, tt.args, "the caller's args are unchanged")
		})
	}
}

func TestCLIRunFailureLogOmitsReplyText(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		args     []string
		wantArgs string
	}{
		{"reply", []string{"comment", "reply", "42", "--body", "Thanks, we fixed it"}, `args="[comment reply 42 --body [redacted]]"`},
		{"other command", []string{"comment", "moderate", "42", "--hide"}, `args="[comment moderate 42 --hide]"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			var ran []string
			stub := &stubSourceStore{}
			stub.runFunc = func(_ context.Context, req CLIRunRequest, _ func(CLIRunEvent) error) error {
				ran = req.Args
				return errors.New("synthetic failure")
			}
			var logs bytes.Buffer
			srv := &Server{store: stub, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			body, err := json.Marshal(CLIRunRequest{Args: tt.args})
			require.NoError(err)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), requestSecurityContextKey{}, requestSecurity{
				auth: requestAuthentication{Mode: AuthModeAPIKey},
			}))
			srv.handleCLIRun(httptest.NewRecorder(), req)

			assert.Equal(tt.args, ran, "the runner gets the real args")
			assert.Contains(logs.String(), "failed to run CLI command")
			assert.Contains(logs.String(), tt.wantArgs)
			assert.NotContains(logs.String(), "we fixed it")
		})
	}
}
