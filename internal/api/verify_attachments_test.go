package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
)

func TestVerifyAttachmentsOwnerAdmission(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()
	args := []string{"verify-attachments", "--json", "--limit=10"}
	assert.True(cliRunCommandAllowed(args))
	assert.False(delegatedCLIRunAdmitted(args, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionAttachmentRead}}), "archive-wide diagnostic remains owner-only")
}

func TestVerifyAttachmentsHTTPCredentialBoundaries(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	assertions := assert.New(t)
	runs := 0
	st := &mockStore{runFunc: func(_ context.Context, req CLIRunRequest, emit func(CLIRunEvent) error) error {
		runs++
		assertions.Equal([]string{"verify-attachments", "--json"}, req.Args)
		requirements.NoError(emit(CLIRunEvent{Type: "stdout", Data: "retained-reference report\n"}))
		return errors.New("attachment verification found incomplete checks")
	}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{
		APIKey: "owner-key", AgentAccess: true,
		RemoteClients: []config.RemoteClientConfig{{ClientID: "reader", APIKey: "remote-key"}},
	}}, st, nil, testLogger())
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionAttachmentRead},
		[]agentgrant.SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}}, time.Time{})
	requirements.NoError(err)
	for _, tc := range []struct {
		name, key, agent string
		wantStatus       int
	}{
		{"owner", "owner-key", "", http.StatusOK},
		{"remote reader", "remote-key", "", http.StatusForbidden},
		{"delegated reader", "", token, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/cli/run",
				strings.NewReader(`{"args":["verify-attachments","--json"]}`))
			req.Header.Set("Content-Type", "application/json")
			if tc.key != "" {
				req.Header.Set("X-Api-Key", tc.key)
			}
			if tc.agent != "" {
				req.Header.Set("X-Msgvault-Agent-Token", tc.agent)
			}
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			require.Equal(tc.wantStatus, resp.Code, resp.Body.String())
			if tc.name == "owner" {
				events := decodeNDJSONEvents[CLIRunEvent](t, resp.Body)
				require.Len(events, 2)
				assert.Equal("stdout", events[0].Type)
				assert.Equal("retained-reference report\n", events[0].Data)
				assert.Equal("error", events[1].Type)
				assert.Equal("attachment verification found incomplete checks", events[1].Error)
			}
		})
	}
	assertions.Equal(1, runs, "restricted credentials must not reach the archive-wide diagnostic")
}
