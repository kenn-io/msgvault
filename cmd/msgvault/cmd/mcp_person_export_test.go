package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
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
		return err
	}
	return emit(api.CLIRunEvent{Type: cliStreamStdout, Data: out.String()})
}

func TestMCPPersonExportThroughRouterAndStore(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("text", "archive")
	require.NoError(t, err)
	participant, err := st.EnsureParticipant("person@example.com", "Example", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	conv := insertExportMessagesConversation(t, st, source.ID, "thread", "Example")
	insertExportMessagesMessage(t, st, source.ID, conv, "selected", start, "synthetic body")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET sender_id = ? WHERE source_id = ?`), participant, source.ID)
	require.NoError(t, err)
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &personExportRouterStore{storeAPIAdapter: &storeAPIAdapter{store: st}, t: t, st: st}, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(fixture.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	caps, err := client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	backend := newDaemonMCPOperations(client, caps)
	assert.Contains(t, backend.capabilities(), "export_person_messages")
	args := map[string]any{"person_id": person.ID, "start": start.Format(time.RFC3339), "end": start.Add(time.Hour).Format(time.RFC3339), "sources": []string{"text:archive"}}
	result, err := backend.ExecuteOperation(t.Context(), "export_person_messages", args)
	require.NoError(t, err)
	require.False(t, result.IsError)
	output, ok := result.Output.(mcpserver.PersonExport)
	require.True(t, ok)
	rows := decodeExportMessageRecords(t, []byte(output.JSONL))
	require.Len(t, rows, 5)
	assert.Equal(t, "selected", rows[3]["id"])
	assert.Contains(t, output.JSONL, `"messages":1`)
	args["end"] = args["start"]
	result, err = backend.ExecuteOperation(t.Context(), "export_person_messages", args)
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestMCPPersonExportReceiptValidation(t *testing.T) {
	start, end, err := parseMessageExportBounds("2026-07-20T00:00:00Z", "2026-07-21T00:00:00Z")
	require.NoError(t, err)
	id := int64(1)
	expected := exportMessagesManifest{Schema: messageExportSchema, Window: exportMessagesWindow{Start: start, End: end}, Filters: exportMessagesFilters{PersonID: &id}}
	manifest := `{"record_type":"manifest","schema":"msgvault-message-export/1","window":{"start":"2026-07-20T00:00:00Z","end":"2026-07-21T00:00:00Z"},"filters":{"person_id":1,"message_types":[],"sources":[]}}` + "\n"
	complete := `{"record_type":"complete","counts":{"sources":0,"conversations":0,"messages":0}}` + "\n"
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"empty complete", manifest + complete, true}, {"truncated", manifest, false}, {"no newline", manifest + strings.TrimSpace(complete), false},
		{"wrong count", manifest + strings.Replace(complete, `"messages":0`, `"messages":1`, 1), false},
		{"missing count", manifest + `{"record_type":"complete","counts":{}}` + "\n", false},
		{"extra record", manifest + complete + complete, false}, {"malformed", manifest + "{\n" + complete, false},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.valid, validPersonExport(tc.body, expected)) })
	}
}

func TestMCPPersonExportRefusesTransportFailureAndOverflow(t *testing.T) {
	for _, tc := range []struct{ name, events, code string }{
		{"failure", `{"type":"error","error":"private detail"}` + "\n", "export_failed"},
		{"overflow", `{"type":"stdout","data":` + strconv.Quote(strings.Repeat("x", mcpPersonExportLimit+1)) + "}\n", "export_limit_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newDaemonCLIRunnerTestServer(t, func(daemonCLIRunTestRequest) {}, tc.events)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(t, err)
			backend := &daemonMCPOperations{client: client, supported: []string{"export_person_messages"}}
			result, err := backend.ExecuteOperation(t.Context(), "export_person_messages", map[string]any{"person_id": 1, "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z"})
			require.NoError(t, err)
			require.True(t, result.IsError)
			encoded, err := json.Marshal(result.Output)
			require.NoError(t, err)
			var failure struct {
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(encoded, &failure))
			assert.Equal(t, tc.code, failure.Error)
			assert.NotContains(t, string(encoded), "private detail")
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

func TestMCPPersonExportCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	export := `{"record_type":"manifest","schema":"msgvault-message-export/1","window":{"start":"2026-07-20T00:00:00Z","end":"2026-07-21T00:00:00Z"},"filters":{"person_id":1,"message_types":[],"sources":[]}}` + "\n" +
		`{"record_type":"complete","counts":{"sources":0,"conversations":0,"messages":0}}` + "\n"
	events := `{"type":"stdout","data":` + strconv.Quote(export) + "}\n" + `{"type":"complete"}` + "\n"
	transport := testTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: cancelOnCloseBody{Reader: strings.NewReader(events), cancel: cancel}}, nil
	})
	client, err := daemonclient.New(daemonclient.Config{URL: "http://127.0.0.1", AllowInsecure: true, HTTPClient: &http.Client{Transport: transport}})
	require.NoError(t, err)
	backend := &daemonMCPOperations{client: client, supported: []string{"export_person_messages"}}
	result, err := backend.ExecuteOperation(ctx, "export_person_messages", map[string]any{"person_id": 1, "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z"})
	require.NoError(t, err)
	require.True(t, result.IsError)
	encoded, err := json.Marshal(result.Output)
	require.NoError(t, err)
	var failure struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(encoded, &failure))
	assert.Equal(t, "export_failed", failure.Error)
}
