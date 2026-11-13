package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type personExportRouterStore struct {
	*storeAPIAdapter

	st        *store.Store
	output    string
	args      []string
	env       map[string]string
	cwd       string
	runExport bool
}

func (s *personExportRouterStore) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	s.args = append([]string(nil), req.Args...)
	s.env = req.Env
	s.cwd = req.Cwd
	if !s.runExport {
		return emit(api.CLIRunEvent{Type: cliStreamStdout, Data: s.output})
	}
	cmd := newExportMessagesLocalCmd(exportMessagesDeps{openStore: func(context.Context) (*store.Store, func(), error) { return s.st, func() {}, nil }})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(req.Args[1:])
	if err := cmd.ExecuteContext(ctx); err != nil {
		if emitErr := emit(api.CLIRunEvent{Type: cliStreamStderr, Data: err.Error()}); emitErr != nil {
			return emitErr
		}
		return fmt.Errorf("execute export messages: %w", err)
	}
	return nil
}

func TestMCPPersonExportThroughRouter(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	start := "2026-07-20T00:00:00Z"
	end := "2026-07-21T00:00:00Z"
	const exportJSONL = "{\"record_type\":\"manifest\",\"schema\":\"msgvault-message-export/1\"}\n{\"record_type\":\"complete\",\"counts\":{\"sources\":0,\"conversations\":0,\"messages\":0}}\n"
	runner := &personExportRouterStore{output: exportJSONL}
	backend := newPersonExportBackendThroughRouter(t, runner)
	assertions.Contains(backend.capabilities(), "export_person_messages")
	args := map[string]any{
		"person_id":     int64(42),
		"start":         start,
		"end":           end,
		"sources":       []string{"text:archive"},
		"message_types": []string{"email"},
	}
	result, err := backend.ExecuteOperation(t.Context(), "export_person_messages", args)
	requirements.NoError(err)
	requirements.False(result.IsError)
	output, ok := result.Output.(mcpserver.PersonExport)
	requirements.True(ok)
	assertions.Equal(strings.Split(exportJSONL, "\n"), strings.Split(output.JSONL, "\n"))
	assertions.Equal([]string{
		"export-messages",
		"--end=" + end,
		"--message-type=email",
		"--person-id=42",
		"--source=text:archive",
		"--start=" + start,
	}, runner.args)
	assertions.Empty(runner.env)
	assertions.Empty(runner.cwd)
}

func TestMCPPersonExportMapsExporterArgumentFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{
			name: "invalid window",
			args: map[string]any{"person_id": int64(42), "start": "2026-07-20T00:00:00Z", "end": "2026-07-20T00:00:00Z"},
		},
		{
			name: "unknown person",
			args: map[string]any{"person_id": int64(42), "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z"},
		},
		{
			name: "unknown source",
			args: map[string]any{"person_id": int64(1), "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z", "sources": []string{"text:missing"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			runner := &personExportRouterStore{st: st, runExport: true}
			backend := newPersonExportBackendThroughRouter(t, runner)
			result, err := backend.ExecuteOperation(t.Context(), "export_person_messages", tc.args)
			requirements.NoError(err)
			requirements.True(result.IsError)
			assertions.Equal("invalid_arguments", personExportFailureCode(t, result.Output))
			encoded, err := json.Marshal(result.Output)
			requirements.NoError(err)
			assertions.NotContains(string(encoded), "person not found")
			assertions.NotContains(string(encoded), "text:missing")
		})
	}
}

func newPersonExportBackendThroughRouter(t *testing.T, runner *personExportRouterStore) *daemonMCPOperations {
	t.Helper()
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: runner, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(fixture.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	caps, err := client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	return newDaemonMCPOperations(client, caps)
}

func TestMCPPersonExportRequiresCLIRouteCapability(t *testing.T) {
	backend := newDaemonMCPOperations(nil, &apiprotocol.MCPCapabilities{Version: 1})
	assert.NotContains(t, backend.capabilities(), "export_person_messages")
}

func TestMCPPersonExportRefusesTransportFailureAndOverflow(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"failure", "export_failed"},
		{"overflow", "export_limit_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			ctx := t.Context()
			cfg := daemonclient.Config{AllowInsecure: true}
			events := []string{`{"type":"error","error":"private detail"}` + "\n"}
			if tc.name == "overflow" {
				event := `{"type":"stdout","data":` + strconv.Quote(strings.Repeat("x", 32<<10)) + "}\n"
				events = make([]string, 17)
				for i := range events {
					events[i] = event
				}
			}
			server, _ := newDaemonCLIRunnerTestServer(t, func(daemonCLIRunTestRequest) {}, events...)
			cfg.URL = server.URL
			client, err := daemonclient.New(cfg)
			requirements.NoError(err)
			backend := &daemonMCPOperations{client: client, supported: []string{"export_person_messages"}}
			result, err := backend.ExecuteOperation(ctx, "export_person_messages", map[string]any{"person_id": 1, "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z"})
			requirements.NoError(err)
			requirements.True(result.IsError)
			assertions.Equal(tc.code, personExportFailureCode(t, result.Output))
			encoded, err := json.Marshal(result.Output)
			requirements.NoError(err)
			assertions.NotContains(string(encoded), "private detail")
		})
	}
}

func personExportFailureCode(t *testing.T, output any) string {
	t.Helper()
	encoded, err := json.Marshal(output)
	require.NoError(t, err)
	var failure struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(encoded, &failure))
	return failure.Error
}
