package cmd

import (
	"bytes"
	"context"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
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
	cmd.SetOut(personExportOutputWriter{emit: emit})
	cmd.SetErr(io.Discard)
	cmd.SetArgs(req.Args[1:])
	if err := cmd.ExecuteContext(ctx); err != nil {
		return fmt.Errorf("execute export messages: %w", err)
	}
	return nil
}

type personExportOutputWriter struct {
	emit func(api.CLIRunEvent) error
}

func (w personExportOutputWriter) Write(data []byte) (int, error) {
	if err := w.emit(api.CLIRunEvent{Type: cliStreamStdout, Data: string(data)}); err != nil {
		return 0, err
	}
	return len(data), nil
}

func TestMCPPersonExportCapsEncodedResponse(t *testing.T) {
	for _, tc := range []struct {
		name        string
		messageBody string
	}{
		{name: "escaped quotes", messageBody: strings.Repeat("\"", 80<<10)},
		{name: "HTML escaping", messageBody: strings.Repeat("<", 42<<10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("text", "archive")
			requirements.NoError(err)
			participant, err := st.EnsureParticipant("sender@example.com", "Example Person", "example.com")
			requirements.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
			conversationID := insertExportMessagesConversation(t, st, source.ID, "large-message", "Example Conversation")
			insertExportMessagesMessage(t, st, source.ID, conversationID, "large-message", start, tc.messageBody)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET sender_id = ? WHERE source_id = ? AND source_message_id = ?`), participant, source.ID, "large-message")
			requirements.NoError(err)

			args := map[string]any{
				"person_id": person.ID,
				"start":     start.Format(time.RFC3339),
				"end":       start.Add(time.Hour).Format(time.RFC3339),
				"sources":   []string{"text:archive"},
			}
			cmd := newExportMessagesLocalCmd(exportMessagesDeps{openStore: func(context.Context) (*store.Store, func(), error) {
				return st, func() {}, nil
			}})
			var native bytes.Buffer
			cmd.SetOut(&native)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{
				"--person-id", strconv.FormatInt(person.ID, 10),
				"--start", start.Format(time.RFC3339),
				"--end", start.Add(time.Hour).Format(time.RFC3339),
				"--source", "text:archive",
			})
			requirements.NoError(cmd.ExecuteContext(t.Context()))
			requirements.Less(native.Len(), mcpPersonExportLimit)

			structured, err := json.Marshal(mcpserver.PersonExport{JSONL: native.String()}, json.Deterministic(true))
			requirements.NoError(err)
			sdkStructured, err := jsonv1.Marshal(jsontext.Value(structured))
			requirements.NoError(err)
			encodedSuccess, err := (&sdkmcp.CallToolResult{
				Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: string(sdkStructured)}},
				StructuredContent: jsontext.Value(sdkStructured),
			}).MarshalJSON()
			requirements.NoError(err)
			encodedSuccess = append([]byte(`{"jsonrpc":"2.0","id":1,"result":`), encodedSuccess...)
			encodedSuccess = append(encodedSuccess, '}')
			requirements.Greater(len(encodedSuccess), mcpPersonExportLimit)

			runner := &personExportRouterStore{st: st}
			fixture := api.NewServerWithOptions(api.ServerOptions{
				Config: &config.Config{}, Store: runner, Logger: slog.New(slog.DiscardHandler),
			})
			backend := sourceOperationFixture(t, fixture)
			result, err := backend.ExecuteOperation(t.Context(), "export_person_messages", args)
			requirements.NoError(err)
			requirements.True(result.IsError)
			assertions.Equal("export_limit_exceeded", personExportFailureCode(t, result.Output))
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
			name: "CLI failure",
			code: "export_failed",
			events: []string{
				`{"type":"error","error":"cli subprocess failed"}`,
			},
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
