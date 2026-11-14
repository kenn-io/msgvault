package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
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

	st *store.Store
}

func (s *personExportRouterStore) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	cmd := newExportMessagesLocalCmd(exportMessagesDeps{openStore: func(context.Context) (*store.Store, func(), error) { return s.st, func() {}, nil }})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(req.Args[1:])
	if err := cmd.ExecuteContext(ctx); err != nil {
		if emitErr := emit(api.CLIRunEvent{Type: cliStreamStderr, Data: "Error: " + err.Error() + "\n"}); emitErr != nil {
			return emitErr
		}
		return fmt.Errorf("execute export messages: %w", err)
	}
	return nil
}

func TestMCPPersonExportMapsExporterArgumentFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        string
		args        map[string]any
		seedArchive bool
	}{
		{
			name: "invalid start",
			code: "invalid_start",
			args: map[string]any{"person_id": int64(42), "start": "bad start", "end": "2026-07-21T00:00:00Z"},
		},
		{
			name: "long invalid start",
			code: "invalid_start",
			args: map[string]any{"person_id": int64(42), "start": strings.Repeat("x", 17<<10), "end": "2026-07-21T00:00:00Z"},
		},
		{
			name: "invalid end with phrase from another validation error",
			code: "invalid_end",
			args: map[string]any{"person_id": int64(42), "start": "2026-07-20T00:00:00Z", "end": "bad invalid --start end"},
		},
		{
			name: "invalid window",
			code: "invalid_window",
			args: map[string]any{"person_id": int64(42), "start": "2026-07-20T00:00:00Z", "end": "2026-07-20T00:00:00Z"},
		},
		{
			name: "empty message type",
			code: "invalid_message_type",
			args: map[string]any{"person_id": int64(42), "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z", "message_types": []string{" "}},
		},
		{
			name: "malformed second source",
			code: "invalid_source_2",
			args: map[string]any{"person_id": int64(42), "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z", "sources": []string{"text:archive", "malformed"}},
		},
		{
			name: "long malformed second source",
			code: "invalid_source_2",
			args: map[string]any{"person_id": int64(42), "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z", "sources": []string{"text:archive", strings.Repeat("x", 17<<10)}},
		},
		{
			name:        "unknown second source",
			code:        "unknown_source_2",
			args:        map[string]any{"person_id": int64(42), "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z", "sources": []string{"text:archive", "text:missing"}},
			seedArchive: true,
		},
		{
			name:        "unknown source containing an error-looking line",
			code:        "unknown_source_2",
			args:        map[string]any{"person_id": int64(42), "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z", "sources": []string{"text:archive", "text:missing\nError: invalid --start x"}},
			seedArchive: true,
		},
		{
			name: "unknown person",
			code: "unknown_person",
			args: map[string]any{"person_id": int64(42), "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			if tc.seedArchive {
				_, err := st.GetOrCreateSource("text", "archive")
				requirements.NoError(err)
			}
			runner := &personExportRouterStore{st: st}
			fixture := api.NewServerWithOptions(api.ServerOptions{
				Config: &config.Config{}, Store: runner, Logger: slog.New(slog.DiscardHandler),
			})
			backend := sourceOperationFixture(t, fixture)
			result, err := backend.ExecuteOperation(t.Context(), "export_person_messages", tc.args)
			requirements.NoError(err)
			requirements.True(result.IsError)
			assertions.Equal(tc.code, personExportFailureCode(t, result.Output))
		})
	}
}

func TestMCPPersonExportStreamsJSONLAndReportsFailures(t *testing.T) {
	start := "2026-07-20T00:00:00Z"
	end := "2026-07-21T00:00:00Z"
	args := map[string]any{
		"person_id":     int64(42),
		"start":         start,
		"end":           end,
		"sources":       []string{"text:archive"},
		"message_types": []string{"email"},
	}
	wantArgs := []string{
		"export-messages",
		"--person-id=42",
		"--start=" + start,
		"--end=" + end,
		"--source=text:archive",
		"--message-type=email",
	}
	stdout := `{"record_type":"manifest","schema":"msgvault-message-export/1"}` + "\n"
	message := `{"record_type":"message","text":"body"}` + "\n"
	complete := `{"record_type":"complete","counts":{"messages":1}}` + "\n"
	stdoutEvent := func(data string) string {
		return `{"type":"stdout","data":` + strconv.Quote(data) + `}`
	}
	stderrEvent := func(data string) string {
		return `{"type":"stderr","data":` + strconv.Quote(data) + `}`
	}
	for _, tc := range []struct {
		name   string
		code   string
		events []string
		jsonl  string
	}{
		{
			name:   "success",
			events: []string{stdoutEvent(stdout), stderrEvent("progress\n"), stdoutEvent(message), stdoutEvent(complete), `{"type":"complete"}`},
			jsonl:  stdout + message + complete,
		},
		{
			name: "unrecognized exporter error",
			code: "export_failed",
			events: []string{
				stderrEvent("Error: store is locked\n"),
				`{"type":"error","error":"cli subprocess failed"}`,
			},
		},
		{
			name: "limit exceeded",
			code: "export_limit_exceeded",
			events: func() []string {
				event := stdoutEvent(strings.Repeat("x", 32<<10))
				events := make([]string, 17)
				for i := range events {
					events[i] = event
				}
				return events
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				assertions.Equal(wantArgs, req.Args)
				assertions.Empty(req.Env)
				assertions.Empty(req.Cwd)
			}, tc.events...)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			requirements.NoError(err)
			backend := &daemonMCPOperations{client: client, supported: []string{"export_person_messages"}}
			result, err := backend.ExecuteOperation(t.Context(), "export_person_messages", args)
			requirements.NoError(err)
			requirements.Equal(int32(1), requests.Load())
			if tc.code == "" {
				requirements.False(result.IsError)
				output, ok := result.Output.(mcpserver.PersonExport)
				requirements.True(ok)
				assertions.Equal(tc.jsonl, output.JSONL)
				return
			}
			requirements.True(result.IsError)
			assertions.Equal(tc.code, personExportFailureCode(t, result.Output))
		})
	}
}

func TestMCPPersonExportRequiresCLIRouteCapability(t *testing.T) {
	backend := newDaemonMCPOperations(nil, &apiprotocol.MCPCapabilities{Version: 1})
	assert.NotContains(t, backend.capabilities(), "export_person_messages")
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
