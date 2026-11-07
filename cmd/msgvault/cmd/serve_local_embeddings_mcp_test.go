//go:build sqlite_vec || pgvector

package cmd

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/query"
)

func TestLocalEmbeddingsActivationPreservesMCPContinuations(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newLocalActivationFixture(t, "")
	f.start(t)
	f.server.SetAnalyticsEngine(query.NewSQLiteEngine(f.store.DB()), "sqlite")
	var messageID int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&messageID))
	require.NoError(f.store.UpsertMessageRaw(messageID, []byte("From: sender@example.test\r\nTo: recipient@example.test\r\nSubject: Orchard\r\n\r\n"+strings.Repeat("Orchard apples. ", 100))))
	participant, err := f.store.EnsureParticipant("contact@example.test", "Observed Example", "example.test")
	require.NoError(err)
	person, _, err := f.store.CreatePersonFromParticipant(participant)
	require.NoError(err)
	command := &cobra.Command{Use: "serve"}
	addServeConfigFlags(command)
	require.NoError(command.Flags().Set("mcp-allow-writes", "true"))
	require.NoError(command.Flags().Set("mcp-allow-profile-writes", "true"))
	require.NoError(installServeMCP(f.ctx, command, f.server, f.state))

	call := func(path, name string, arguments map[string]any, state any) map[string]any {
		params := map[string]any{"name": name, "arguments": arguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}
		if state != nil {
			params["requestState"] = state
			params["inputResponses"] = map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}}
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
		require.NoError(err)
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(string(body)))
		request.Header.Set("Authorization", "Bearer owner")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", "tools/call")
		request.Header.Set("Mcp-Name", name)
		request.RemoteAddr = "127.0.0.1:1234"
		response := httptest.NewRecorder()
		f.server.Router().ServeHTTP(response, request)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		var value struct {
			Result map[string]any `json:"result"`
		}
		require.NoError(json.Unmarshal(response.Body.Bytes(), &value))
		require.NotNil(value.Result)
		return value.Result
	}
	edit := call("/mcp", "get_person_edit_context", map[string]any{"person_id": person.ID}, nil)
	require.NotEqual(true, edit["isError"], "%v", edit["content"])
	editContext, ok := edit["structuredContent"].(map[string]any)
	require.True(ok)
	arguments := map[string]any{"person_id": person.ID, "etag": editContext["etag"], "display_name": "Curated Example"}
	pending := call("/mcp/writes", "set_person_display_name", arguments, nil)
	require.NotEmpty(pending["requestState"], "%v", pending["content"])
	first := call("/mcp", "export_eml", map[string]any{"id": messageID, "offset": 0, "length": 100}, nil)
	require.NotEqual(true, first["isError"], "%v", first["content"])
	chunk, ok := first["structuredContent"].(map[string]any)
	require.True(ok)
	require.Equal(false, chunk["complete"])
	require.NoError(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
	accepted := call("/mcp/writes", "set_person_display_name", arguments, pending["requestState"])
	assert.NotEqual(true, accepted["isError"], "%v", accepted["content"])
	second := call("/mcp", "export_eml", map[string]any{"id": messageID, "offset": chunk["length"], "length": 100, "sha256": chunk["sha256"]}, nil)
	assert.NotEqual(true, second["isError"], "%v", second["content"])
	semantic := call("/mcp", "semantic_search_messages", map[string]any{"query": "orchard", "mode": "vector"}, nil)
	require.NotEqual(true, semantic["isError"], "%v", semantic["content"])
	encoded, err := json.Marshal(semantic["structuredContent"])
	require.NoError(err)
	assert.Contains(string(encoded), "Orchard")
}
