package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/cligrammar"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
)

func TestCLIGrammarAdmission(t *testing.T) {
	for _, move := range cligrammar.Moves() {
		t.Run(move.Legacy, func(t *testing.T) {
			old := []string{move.Legacy, "42"}
			canonical := append(strings.Fields(move.Canonical), "42")
			assert.Equal(t, CLIRunCommandAllowed(old), CLIRunCommandAllowed(canonical))
			for _, permission := range []agentgrant.Permission{
				"", agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete,
			} {
				grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{permission}}
				assert.Equal(t, delegatedCLIRunAdmitted(old, grant), delegatedCLIRunAdmitted(canonical, grant))
			}
		})
	}
	for _, tc := range []struct {
		args               []string
		allowed, delegated bool
	}{
		{[]string{"draft", "reply", "42"}, true, true},
		{[]string{"draft", "compose", "user@example.com"}, true, true},
		{[]string{"draft", "get", "draft-example"}, true, true},
		{[]string{"draft", "forward", "42"}, true, false},
		{[]string{"draft", "send-as", "user@example.com"}, true, false},
		{[]string{"source", "calendar", "add", "user@example.com"}, true, false},
		{[]string{"source", "slack", "unknown"}, false, false},
		{[]string{"draft", "--json", "reply", "42"}, false, false},
		{[]string{"source", "slack", "sync", "__worker"}, false, false},
	} {
		assert.Equal(t, tc.allowed, CLIRunCommandAllowed(tc.args), "%v", tc.args)
		assert.Equal(t, tc.delegated, delegatedCLIRunAdmitted(tc.args, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}), "%v", tc.args)
	}
}

func TestCLIGrammarGate(t *testing.T) {
	for _, tc := range []struct {
		path  string
		skip  bool
		label string
	}{
		{"draft get", true, ""},
		{"draft send-as", true, ""},
		{"deletion list", true, ""},
		{"deletion show", true, ""},
		{"draft edit", false, "msgvault draft edit"},
		{"source slack sync", false, "msgvault source slack sync"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			assert := assert.New(t)
			body, err := json.Marshal(CLIRunRequest{Args: append(strings.Fields(tc.path), "42")})
			require.NoError(t, err)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(string(body)))
			label, skip, err := cliRunGateDecision(r, requestAuthentication{})
			require.NoError(t, err)
			assert.Equal(tc.skip, skip)
			assert.Equal(tc.label, label)
		})
	}
}

func TestCLIGrammarCredentialBoundaries(t *testing.T) {
	assert := assert.New(t)
	srv := &Server{}
	assert.True(srv.cliRunEnvAllowedForCommand([]string{"source", "inline", "add"}, clirun.EnvInlineOAuth))
	assert.False(srv.cliRunEnvAllowedForCommand([]string{"source", "slack", "add"}, clirun.EnvInlineOAuth))
	assert.False(srv.cliRunEnvAllowedForCommand([]string{"source", "inline", "sync"}, clirun.EnvInlineOAuth))
	assert.False(srv.cliRunEnvAllowedForCommand([]string{"draft", "reply", "42"}, clirun.EnvInlineOAuth))
}

func TestCLIGrammarGatePreservesEmbeddingSlotKind(t *testing.T) {
	for _, tc := range []struct {
		old, canonical, kind string
	}{
		{"sync-slack", "source slack sync", "sync"},
		{"sync-calendar", "source calendar sync", "sync"},
		{"import-mbox", "import mbox", "other"},
		{"build-embeddings", "embeddings build", "embeddings"},
	} {
		for _, path := range []string{tc.old, tc.canonical} {
			t.Run(path, func(t *testing.T) {
				require := require.New(t)
				body, err := json.Marshal(CLIRunRequest{Args: strings.Fields(path)})
				require.NoError(err)
				request := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(string(body)))
				label, skip, err := cliRunGateDecision(request, requestAuthentication{})
				require.NoError(err)
				require.False(skip)
				assert.Equal(t, tc.kind, embeddingSlotKind(label, true))
			})
		}
	}
}

func TestCLIGrammarRuntimeProof(t *testing.T) {
	for _, path := range [][]string{{"add-account"}, {"source", "gmail", "add"}} {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var got CLIRunRequest
			st := &mockStore{runFunc: func(_ context.Context, req CLIRunRequest, emit func(CLIRunEvent) error) error {
				got = req
				return emit(CLIRunEvent{Type: cliStreamEventTypeComplete})
			}}
			srv := NewServerWithOptions(ServerOptions{
				Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
				Store:  st, Logger: testLogger(), ShutdownToken: "synthetic-runtime-secret",
			})
			body, err := json.Marshal(CLIRunRequest{Args: append(path, "user@example.com", "--readonly")})
			require.NoError(err)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(apiprotocol.DaemonRuntimeTokenHeader, "synthetic-runtime-secret")
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			require.Equal(http.StatusOK, resp.Code, "%s", resp.Body.String())
			assert.Equal([]string{"add-account", "user@example.com", "--readonly"}, got.Args)
			assert.True(got.GrantDecided)
		})
	}
}
