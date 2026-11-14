package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
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
	stdoutEvent := func(data string) string {
		return `{"type":"stdout","data":` + strconv.Quote(data) + `}`
	}
	stderrEvent := func(data string) string {
		return `{"type":"stderr","data":` + strconv.Quote(data) + `}`
	}
	const subprocessFailureEvent = `{"type":"error","error":"msgvault: cli subprocess exited non-zero"}`
	var oversizedEvents []string
	for remaining := mcpPersonExportLimit + 1; remaining > 0; {
		chunkSize := min(32<<10, remaining)
		oversizedEvents = append(oversizedEvents, stdoutEvent(strings.Repeat("x", chunkSize)))
		remaining -= chunkSize
	}
	oversizedEvents = append(oversizedEvents, subprocessFailureEvent)
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
			name: "CLI failure",
			code: "export_failed",
			events: []string{
				stderrEvent("Error: store is locked\n"),
				subprocessFailureEvent,
			},
		},
		{
			name:   "raw stdout over limit",
			code:   "export_limit_exceeded",
			events: oversizedEvents,
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
			encoded, err := json.Marshal(result.Output)
			requirements.NoError(err)
			assertions.JSONEq(`{"error":"`+tc.code+`","operation_may_have_completed":false}`, string(encoded))
			if tc.name == "CLI failure" {
				assertions.NotContains(string(encoded), "store is locked")
			}
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

func TestMCPPersonExportCapsEncodedSDKResponse(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("text", "archive")
	requirements.NoError(err)
	participant, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	start := "2026-07-20T00:00:00Z"
	end := "2026-07-21T00:00:00Z"
	startTime, err := time.Parse(time.RFC3339, start)
	requirements.NoError(err)
	conversation := insertExportMessagesConversation(t, st, source.ID, "conversation", "Example")
	insertExportMessagesMessage(t, st, source.ID, conversation, "message", startTime, strings.Repeat("c", 258800))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET sender_id = ? WHERE source_id = ?`), participant, source.ID)
	requirements.NoError(err)
	command := newExportMessagesLocalCmd(exportMessagesDeps{openStore: func(context.Context) (*store.Store, func(), error) {
		return st, func() {}, nil
	}})
	var native bytes.Buffer
	command.SetOut(&native)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--start", start, "--end", end, "--person-id", strconv.FormatInt(person.ID, 10)})
	requirements.NoError(command.ExecuteContext(t.Context()))
	stdoutEvent := `{"type":"stdout","data":` + strconv.Quote(native.String()) + `}`
	server, _ := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assertions.Equal([]string{
			"export-messages", "--person-id=" + strconv.FormatInt(person.ID, 10), "--start=" + start, "--end=" + end,
		}, req.Args)
	}, stdoutEvent, `{"type":"complete"}`)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	backend := &daemonMCPOperations{client: client, supported: []string{"export_person_messages"}}
	ctx, cancel := context.WithCancel(t.Context())
	directory := filepath.Join(t.TempDir(), "mcp")
	done := make(chan error, 1)
	go func() {
		done <- mcpserver.ServeHTTPWithOptions(ctx, mcpserver.ServeOptions{
			Operations: backend, OperationCapabilities: []string{"export_person_messages"},
		}, mcpserver.HTTPOptions{Addr: "127.0.0.1:0", DiscoveryDirectory: directory, AllowWrites: true})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assertions.ErrorIs(err, context.Canceled)
		case <-time.After(10 * time.Second):
			assertions.Fail("MCP listener did not stop")
		}
	})
	var endpoints []mcpdiscovery.Endpoint
	requirements.Eventually(func() bool {
		var err error
		endpoints, err = mcpdiscovery.List(directory)
		return err == nil && len(endpoints) == 1
	}, 10*time.Second, 10*time.Millisecond)
	session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-client", Version: "1"}, nil).Connect(
		ctx, &sdkmcp.StreamableClientTransport{Endpoint: endpoints[0].URL}, nil,
	)
	requirements.NoError(err)
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "export_person_messages",
		Arguments: map[string]any{
			"person_id": person.ID, "start": start, "end": end,
		},
	})
	requirements.NoError(err)
	encoded, err := json.Marshal(result)
	requirements.NoError(err)
	assertions.LessOrEqual(len(encoded), mcpPersonExportLimit)
	assertions.True(result.IsError)
	assertions.Contains(string(encoded), "export_limit_exceeded")
}
