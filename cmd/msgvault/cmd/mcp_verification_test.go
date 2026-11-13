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
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
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

func TestOwnerMCPVerificationStageDeletion(t *testing.T) {
	f := storetest.New(t)
	f.PersistEmail(storetest.EmailSpec{From: "sender@example.test", To: []string{storetest.OwnerAddress}, Subject: "Ordinary message", Body: "Hello"})
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner"}}
	adapter := &storeAPIAdapter{store: f.Store, config: cfg}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Engine: query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, server.Shutdown(context.WithoutCancel(t.Context()))) })
	daemon := httptest.NewServer(server.Router())
	t.Cleanup(daemon.Close)
	cfg.Remote = config.RemoteConfig{URL: daemon.URL, APIKey: "owner", AllowInsecure: true}
	setMCPTokenTestFlags(t, nil)
	old := mcpRevealVerification
	mcpRevealVerification = false
	t.Cleanup(func() { mcpRevealVerification = old })
	session := mcpDraftTestSession(withStoreResolverConfig(t, cfg), t)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "stage_deletion", Arguments: map[string]any{"from": "sender@example.test"}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result.Content)
	manager, err := deletion.NewManager(filepath.Join(cfg.Data.DataDir, "deletions"))
	require.NoError(t, err)
	manifests, err := manager.ListPending()
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	assert.Equal(t, []string{"email-1"}, manifests[0].GmailIDs)
}

func TestOwnerMCPVerificationPersonProfile(t *testing.T) {
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("person@example.test", "Example Person", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner"}}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, server.Shutdown(context.WithoutCancel(t.Context()))) })
	daemon := httptest.NewServer(server.Router())
	t.Cleanup(daemon.Close)
	cfg.Remote = config.RemoteConfig{URL: daemon.URL, APIKey: "owner", AllowInsecure: true}
	setMCPTokenTestFlags(t, nil)
	old := mcpRevealVerification
	mcpRevealVerification = false
	t.Cleanup(func() { mcpRevealVerification = old })
	session := mcpDraftTestSession(withStoreResolverConfig(t, cfg), t)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_profile", Arguments: map[string]any{"person_id": person.ID}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result.Content)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	assert.Contains(t, string(data), "Example Person")
	assert.Contains(t, string(data), `"brief":null`)
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
			} else {
				assert.True(t, result.IsError)
				assert.NotContains(t, text, "482913")
				assert.Contains(t, text, "verification content is withheld")
			}
			stats, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_stats", Arguments: map[string]any{}})
			require.NoError(t, err)
			assert.False(t, stats.IsError)
			created, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_saved_view", Arguments: map[string]any{"name": "new view", "canonical_state": map[string]any{"presentation": "table"}, "schema_version": 1}})
			require.NoError(t, err)
			require.False(t, created.IsError, "%+v", created.Content)
			definition, ok := created.StructuredContent.(map[string]any)
			require.True(t, ok)
			updated, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_saved_view", Arguments: map[string]any{"id": definition["id"], "revision": definition["revision"], "name": "updated view"}})
			require.NoError(t, err)
			assert.False(t, updated.IsError, "%+v", updated.Content)
		})
	}
}
