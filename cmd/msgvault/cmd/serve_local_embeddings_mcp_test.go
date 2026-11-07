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
)

func TestLocalEmbeddingsActivationEnablesMCPSearch(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newLocalActivationFixture(t, "")
	f.start(t)
	command := &cobra.Command{Use: "serve"}
	addServeConfigFlags(command)
	require.NoError(installServeMCP(f.ctx, command, f.server, f.state))
	require.NoError(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"semantic_search_messages","arguments":{"query":"orchard","mode":"vector"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer owner")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", "semantic_search_messages")
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	f.server.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var value struct {
		Result struct {
			IsError bool `json:"isError"`
			Content any  `json:"structuredContent"`
		} `json:"result"`
	}
	require.NoError(json.Unmarshal(response.Body.Bytes(), &value))
	require.False(value.Result.IsError, response.Body.String())
	encoded, err := json.Marshal(value.Result.Content)
	require.NoError(err)
	assert.Contains(string(encoded), "Orchard")
}
