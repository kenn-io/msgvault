package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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

	t  *testing.T
	st *store.Store
}

func (s *personExportRouterStore) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	require.Equal(s.t, "export-messages", req.Args[0])
	require.Empty(s.t, req.Env)
	require.Empty(s.t, req.Cwd)
	cmd := newExportMessagesLocalCmd(exportMessagesDeps{openStore: func(context.Context) (*store.Store, func(), error) { return s.st, func() {}, nil }})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(req.Args[1:])
	if err := cmd.ExecuteContext(ctx); err != nil {
		return fmt.Errorf("execute export messages: %w", err)
	}
	return emit(api.CLIRunEvent{Type: cliStreamStdout, Data: out.String()})
}

func TestMCPPersonExportThroughRouterAndStore(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("text", "archive")
	requirements.NoError(err)
	participant, err := st.EnsureParticipant("person@example.com", "Example", "example.com")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	conv := insertExportMessagesConversation(t, st, source.ID, "thread", "Example")
	insertExportMessagesMessage(t, st, source.ID, conv, "selected", start, "synthetic body")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET sender_id = ? WHERE source_id = ?`), participant, source.ID)
	requirements.NoError(err)
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &personExportRouterStore{storeAPIAdapter: &storeAPIAdapter{store: st}, t: t, st: st}, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(fixture.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	caps, err := client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	backend := newDaemonMCPOperations(client, caps)
	assertions.Contains(backend.capabilities(), "export_person_messages")
	args := map[string]any{"person_id": person.ID, "start": start.Format(time.RFC3339), "end": start.Add(time.Hour).Format(time.RFC3339), "sources": []string{"text:archive"}}
	result, err := backend.ExecuteOperation(t.Context(), "export_person_messages", args)
	requirements.NoError(err)
	requirements.False(result.IsError)
	output, ok := result.Output.(mcpserver.PersonExport)
	requirements.True(ok)
	rows := decodeExportMessageRecords(t, []byte(output.JSONL))
	requirements.Len(rows, 5)
	assertions.Equal("selected", rows[3]["id"])
	assertions.Contains(output.JSONL, `"messages":1`)
	args["end"] = args["start"]
	result, err = backend.ExecuteOperation(t.Context(), "export_person_messages", args)
	requirements.NoError(err)
	assertions.True(result.IsError)
	assertions.Equal("export_failed", personExportFailureCode(t, result.Output))
}

func TestMCPPersonExportRequiresCLIRouteCapability(t *testing.T) {
	backend := newDaemonMCPOperations(nil, &apiprotocol.MCPCapabilities{Version: 1})
	assert.NotContains(t, backend.capabilities(), "export_person_messages")
}

func TestMCPPersonExportRefusesTransportFailureAndOverflow(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"failure", "export_failed"},
		{"overflow", "export_limit_exceeded"},
		{"cancelled", "export_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			ctx := t.Context()
			cfg := daemonclient.Config{AllowInsecure: true}
			if tc.name == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				export := `{"record_type":"manifest","schema":"msgvault-message-export/1","msgvault_version":"test","window":{"start":"2026-07-20T00:00:00Z","end":"2026-07-21T00:00:00Z"},"filters":{"person_id":1,"message_types":[],"sources":[]}}` + "\n" +
					`{"record_type":"complete","counts":{"sources":0,"conversations":0,"messages":0}}` + "\n"
				events := `{"type":"stdout","data":` + strconv.Quote(export) + "}\n" + `{"type":"complete"}` + "\n"
				transport := testTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: cancelOnCloseBody{Reader: strings.NewReader(events), cancel: cancel}}, nil
				})
				cfg.URL = "http://127.0.0.1"
				cfg.HTTPClient = &http.Client{Transport: transport}
			} else {
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
			}
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

type cancelOnCloseBody struct {
	io.Reader

	cancel context.CancelFunc
}

func (b cancelOnCloseBody) Close() error {
	b.cancel()
	return nil
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
