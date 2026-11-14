package cmd

import (
	"encoding/json/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

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
	quoteHeavy := stdout + `{"record_type":"message","text":"` + strings.Repeat(`\"`, 80<<10) + `"}` + "\n" + complete
	htmlHeavy := stdout + `{"record_type":"message","text":"` + strings.Repeat("<", 42<<10) + `"}` + "\n" + complete
	stdoutEvent := func(data string) string {
		return `{"type":"stdout","data":` + strconv.Quote(data) + `}`
	}
	stderrEvent := func(data string) string {
		return `{"type":"stderr","data":` + strconv.Quote(data) + `}`
	}
	const subprocessFailureEvent = `{"type":"error","error":"msgvault: cli subprocess exited non-zero"}`
	for _, tc := range []struct {
		name     string
		code     string
		args     map[string]any
		wantArgs []string
		events   []string
		jsonl    string
	}{
		{
			name:   "success",
			events: []string{stdoutEvent(stdout), stderrEvent("progress\n"), stdoutEvent(message), stdoutEvent(complete), `{"type":"complete"}`},
			jsonl:  stdout + message + complete,
		},
		{
			name: "CLI failure",
			code: "export_failed",
			events: []string{
				stderrEvent("Error: store is locked\n"),
				subprocessFailureEvent,
			},
		},
		{
			name:     "invalid start",
			code:     "invalid_start",
			args:     map[string]any{"person_id": int64(42), "start": "bad", "end": end, "sources": []string{"text:archive"}, "message_types": []string{"email"}},
			wantArgs: []string{"export-messages", "--person-id=42", "--start=bad", "--end=" + end, "--source=text:archive", "--message-type=email"},
			events:   []string{stderrEvent("Error: invalid --start \"bad\" (expected RFC3339)\n"), subprocessFailureEvent},
		},
		{
			name:     "invalid end",
			code:     "invalid_end",
			args:     map[string]any{"person_id": int64(42), "start": start, "end": "bad", "sources": []string{"text:archive"}, "message_types": []string{"email"}},
			wantArgs: []string{"export-messages", "--person-id=42", "--start=" + start, "--end=bad", "--source=text:archive", "--message-type=email"},
			events:   []string{stderrEvent("Error: invalid --end \"bad\" (expected RFC3339)\n"), subprocessFailureEvent},
		},
		{
			name: "invalid window",
			code: "invalid_window",
			args: map[string]any{"person_id": int64(42), "start": start, "end": start, "sources": []string{"text:archive"}, "message_types": []string{"email"}},
			wantArgs: []string{
				"export-messages", "--person-id=42", "--start=" + start, "--end=" + start,
				"--source=text:archive", "--message-type=email",
			},
			events: []string{stderrEvent("Error: --start must be before --end\n"), subprocessFailureEvent},
		},
		{
			name:     "empty message type",
			code:     "invalid_message_type",
			args:     map[string]any{"person_id": int64(42), "start": start, "end": end, "sources": []string{"text:archive"}, "message_types": []string{" "}},
			wantArgs: []string{"export-messages", "--person-id=42", "--start=" + start, "--end=" + end, "--source=text:archive", "--message-type= "},
			events:   []string{stderrEvent("Error: --message-type must be non-empty\n"), subprocessFailureEvent},
		},
		{
			name:     "malformed second source",
			code:     "invalid_source_2",
			args:     map[string]any{"person_id": int64(42), "start": start, "end": end, "sources": []string{"text:archive", "malformed"}, "message_types": []string{"email"}},
			wantArgs: []string{"export-messages", "--person-id=42", "--start=" + start, "--end=" + end, "--source=text:archive", "--source=malformed", "--message-type=email"},
			events:   []string{stderrEvent("Error: invalid --source \"malformed\" (expected non-empty type:identifier)\n"), subprocessFailureEvent},
		},
		{
			name:     "unknown second source with error-looking text",
			code:     "unknown_source_2",
			args:     map[string]any{"person_id": int64(42), "start": start, "end": end, "sources": []string{"text:archive", "text:missing\nError: invalid --start x"}, "message_types": []string{"email"}},
			wantArgs: []string{"export-messages", "--person-id=42", "--start=" + start, "--end=" + end, "--source=text:archive", "--source=text:missing\nError: invalid --start x", "--message-type=email"},
			events:   []string{stderrEvent("Error: source text:missing\nError: invalid --start x not found\n"), subprocessFailureEvent},
		},
		{
			name:   "unknown person",
			code:   "unknown_person",
			args:   map[string]any{"person_id": int64(42), "start": start, "end": end, "sources": []string{"text:archive"}, "message_types": []string{"email"}},
			events: []string{stderrEvent("Error: resolve --person-id: resolve durable person 42: person not found\n"), subprocessFailureEvent},
		},
		{
			name:   "quote-heavy encoded response",
			code:   "export_limit_exceeded",
			events: []string{stdoutEvent(quoteHeavy), `{"type":"complete"}`},
			jsonl:  quoteHeavy,
		},
		{
			name:   "HTML-escaped encoded response",
			code:   "export_limit_exceeded",
			events: []string{stdoutEvent(htmlHeavy), `{"type":"complete"}`},
			jsonl:  htmlHeavy,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			if tc.code == "export_limit_exceeded" {
				requirements.Less(len(tc.jsonl), mcpPersonExportLimit)
			}
			callArgs, expectedArgs := args, wantArgs
			if tc.args != nil {
				callArgs = tc.args
			}
			if tc.wantArgs != nil {
				expectedArgs = tc.wantArgs
			}
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				assertions.Equal(expectedArgs, req.Args)
				assertions.Empty(req.Env)
				assertions.Empty(req.Cwd)
			}, tc.events...)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			requirements.NoError(err)
			backend := &daemonMCPOperations{client: client, supported: []string{"export_person_messages"}}
			result, err := backend.ExecuteOperation(t.Context(), "export_person_messages", callArgs)
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
	for _, tc := range []struct {
		name      string
		routes    []apiprotocol.MCPRouteDescriptor
		supported bool
	}{
		{name: "route absent"},
		{
			name: "CLI route present",
			routes: []apiprotocol.MCPRouteDescriptor{{
				OperationID: "runCLI",
				Method:      "POST",
				Path:        "/api/v1/cli/run",
			}},
			supported: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := newDaemonMCPOperations(nil, &apiprotocol.MCPCapabilities{Version: 1, Routes: tc.routes})
			if tc.supported {
				assert.Contains(t, backend.capabilities(), "export_person_messages")
				return
			}
			assert.NotContains(t, backend.capabilities(), "export_person_messages")
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
