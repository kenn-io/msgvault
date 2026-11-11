package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOwnerMCPVerificationReads(t *testing.T) {
	for _, reveal := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "reveal"}[reveal], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			_, id, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "reader@example.test", "code", "Your verification code is 482913", "Your verification code is 482913")
			require.NoError(err)
			cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner"}}
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
			t.Cleanup(func() { require.NoError(server.Shutdown(context.WithoutCancel(t.Context()))) })
			cmd := &cobra.Command{Use: "serve"}
			addServeConfigFlags(cmd)
			if reveal {
				require.NoError(cmd.Flags().Set("mcp-reveal-verification", "true"))
			}
			require.NoError(installServeMCP(t.Context(), cmd, server, &invocation{cfg: cfg}))
			payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_message","arguments":{"id":%d,"full_body":true}}}`, id)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(payload))
			req.Header.Set("Authorization", "Bearer owner")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, req)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			assert.NotContains(response.Body.String(), `"isError":true`)
			if reveal {
				assert.Contains(response.Body.String(), "482913")
			} else {
				assert.NotContains(response.Body.String(), "482913")
				assert.Contains(response.Body.String(), "verification content masked")
			}
		})
	}
}
