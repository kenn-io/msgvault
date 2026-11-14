package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
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

func TestOwnerMCPVerificationSavedView(t *testing.T) {
	for _, reveal := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "reveal"}[reveal], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "archive.db")
			st := testutil.NewSQLiteTestStoreAt(t, path)
			_, id, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "reader@example.test", "code", "Your verification code is 482913", "Your verification code is 482913")
			require.NoError(t, err)
			_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET sent_at = '2026-07-01T00:00:00Z' WHERE id = ?`, id)
			require.NoError(t, err)
			view, err := st.CreateSavedView(t.Context(), store.SavedViewInput{Name: "codes", CanonicalState: []byte(`{"presentation":"table"}`), SchemaVersion: 1})
			require.NoError(t, err)
			cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner"}}
			engine := query.NewLiveExploreEngine(st.DB(), path)
			t.Cleanup(func() { require.NoError(t, engine.Close()) })
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, SavedViewStore: st, Engine: engine, AnalyticsMode: api.AnalyticsModeSQLFallback, Logger: slog.New(slog.DiscardHandler)})
			t.Cleanup(func() { require.NoError(t, server.Shutdown(context.WithoutCancel(t.Context()))) })
			daemon := httptest.NewServer(server.Router())
			t.Cleanup(daemon.Close)
			cfg.Remote = config.RemoteConfig{URL: daemon.URL, APIKey: "owner", AllowInsecure: true}
			setMCPTokenTestFlags(t, nil)
			old := mcpRevealVerification
			mcpRevealVerification = reveal
			t.Cleanup(func() { mcpRevealVerification = old })
			session := mcpDraftTestSession(withStoreResolverConfig(t, cfg), t)
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "run_saved_view", Arguments: map[string]any{"id": view.ID}})
			require.NoError(t, err)
			encoded, err := json.Marshal(result.Content)
			require.NoError(t, err)
			text := string(encoded)
			if reveal {
				assert.False(t, result.IsError)
				assert.Contains(t, text, "482913")
				return
			}
			assert.True(t, result.IsError)
			assert.Contains(t, text, "verification content is withheld")
			assert.NotContains(t, text, "482913")

		})
	}
}
