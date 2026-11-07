//go:build sqlite_vec || pgvector

package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/vector"
)

func TestLocalEmbeddingsActivationEnablesMCPSearch(t *testing.T) {
	for _, transport := range []string{"native", "standalone"} {
		t.Run(transport, func(t *testing.T) {
			checks, must := assert.New(t), require.New(t)
			f := newLocalActivationFixture(t, "")
			f.start(t)
			var handler http.Handler
			if transport == "native" {
				command := &cobra.Command{Use: "serve"}
				addServeConfigFlags(command)
				must.NoError(installServeMCP(f.ctx, command, f.server, f.state))
				handler = f.server.Router()
			} else {
				daemon := httptest.NewServer(f.server.Router())
				t.Cleanup(daemon.Close)
				client, err := newDaemonCLIClient(f.ctx, daemonclient.Config{URL: daemon.URL, APIKey: "owner", AllowInsecure: true})
				must.NoError(err)
				t.Cleanup(func() { must.NoError(client.Close()) })
				var closeHandler func()
				handler, closeHandler = mcpserver.NewHTTPHandler(daemonMCPServeOptions(f.ctx, client, f.state), mcpserver.HTTPOptions{})
				t.Cleanup(closeHandler)
			}
			f.server.SetLocalEmbeddings(f.controller)
			activation := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/embeddings/local", nil)
			activation.Header.Set("Authorization", "Bearer owner")
			activation.Header.Set("Content-Type", "application/json")
			activation.RemoteAddr = "127.0.0.1:1234"
			admitted := httptest.NewRecorder()
			f.server.Router().ServeHTTP(admitted, activation)
			must.Equal(http.StatusAccepted, admitted.Code, admitted.Body.String())
			must.Eventually(func() bool {
				request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/embeddings/local", nil)
				request.Header.Set("Authorization", "Bearer owner")
				request.RemoteAddr = "127.0.0.1:1234"
				response := httptest.NewRecorder()
				f.server.Router().ServeHTTP(response, request)
				checks.Equal(http.StatusOK, response.Code, response.Body.String())
				var status api.LocalEmbeddingsStatus
				checks.NoError(json.Unmarshal(response.Body.Bytes(), &status))
				checks.Empty(status.Error)
				return status.Enabled && !status.Running
			}, 10*time.Second, 10*time.Millisecond)

			for _, tool := range []struct {
				name      string
				arguments map[string]any
				want      string
			}{
				{name: "semantic_search_messages", arguments: map[string]any{"query": "orchard", "mode": "vector"}, want: "Orchard"},
				{name: "get_embeddings_status", arguments: map[string]any{}},
			} {
				t.Run(tool.name, func(t *testing.T) {
					assert, require := assert.New(t), require.New(t)
					body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool.name, "arguments": tool.arguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}})
					require.NoError(err)
					request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(string(body)))
					request.Header.Set("Authorization", "Bearer owner")
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Accept", "application/json, text/event-stream")
					request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
					request.Header.Set("Mcp-Method", "tools/call")
					request.Header.Set("Mcp-Name", tool.name)
					request.RemoteAddr = "127.0.0.1:1234"
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					require.Equal(http.StatusOK, response.Code, response.Body.String())
					var value struct {
						Result struct {
							IsError bool           `json:"isError"`
							Content jsontext.Value `json:"structuredContent"`
						} `json:"result"`
					}
					require.NoError(json.Unmarshal(response.Body.Bytes(), &value))
					require.False(value.Result.IsError, response.Body.String())
					if tool.name == "get_embeddings_status" {
						var status vector.EmbeddingStatus
						require.NoError(json.Unmarshal(value.Result.Content, &status))
						require.Equal("active", status.Generation.State)
						require.Equal(int64(1), status.Current)
					} else {
						assert.Contains(string(value.Result.Content), tool.want)
					}
				})
			}
		})
	}
}
