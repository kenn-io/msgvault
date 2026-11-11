package cmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOwnerMCPVerificationReads(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, id, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "reader@example.test", "code", "Your verification code is 482913", "Your verification code is 482913")
	require.NoError(t, err)
	_, ordinary, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "reader@example.test", "ordinary", "Project update", "Project update")
	require.NoError(t, err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner"}}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, server.Shutdown(context.WithoutCancel(t.Context()))) })
	daemon := httptest.NewServer(server.Router())
	t.Cleanup(daemon.Close)
	for _, reveal := range []bool{false, true} {
		t.Run(map[bool]string{false: "withhold", true: "reveal"}[reveal], func(t *testing.T) {
			client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: "owner", AllowInsecure: true})
			require.NoError(t, err)
			if !reveal {
				client, err = client.WithVerificationWithheld()
				require.NoError(t, err)
			}
			opts := daemonMCPServeOptions(t.Context(), client, &invocation{cfg: cfg})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ct, transport := sdkmcp.NewInMemoryTransports()
			done := make(chan error, 1)
			go func() { done <- mcpserver.ServeTransport(ctx, opts, transport) }()
			session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "verification-test", Version: "1"}, nil).Connect(ctx, ct, nil)
			require.NoError(t, err)
			for _, args := range []map[string]any{{"id": id, "full_body": true}, {"id": id, "center_at": 5, "max_chars": 100}, {"id": id, "body_format": "html"}, {"id": ordinary, "full_body": true}} {
				result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "get_message", Arguments: args})
				require.NoError(t, err)
				require.False(t, result.IsError)
				body, err := json.Marshal(result)
				require.NoError(t, err)
				if args["id"] == ordinary {
					assert.Contains(t, string(body), "Project update")
				} else if reveal {
					assert.Contains(t, string(body), "482913")
				} else {
					assert.NotContains(t, string(body), "482913")
					assert.Contains(t, string(body), "verification content masked")
				}
			}
			require.NoError(t, session.Close())
			cancel()
			if err := <-done; err != nil {
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}
