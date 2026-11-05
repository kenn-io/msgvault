package cmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
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
	"go.kenn.io/msgvault/internal/store"
)

// Uses the production cache built from three synthetic provider sync snapshots.
// Both listeners and the SDK exercise the actual serialized transport contracts.
func assertProviderMailboxSavedViewMCP(t *testing.T, st *store.Store, path, analytics string) {
	t.Helper()
	requirements := require.New(t)
	assertions := assert.New(t)

	engine, err := query.NewDuckDBEngine(analytics, path, st.DB())
	requirements.NoError(err)
	defer func() { _ = engine.Close() }()
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-mailbox-owner"}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, SavedViewStore: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
	defer func() { _ = srv.Shutdown(context.Background()) }()
	daemon := httptest.NewServer(srv.Router())
	defer daemon.Close()
	response, err := http.Get(daemon.URL + "/api/v1/saved-views")
	requirements.NoError(err)
	requirements.NoError(response.Body.Close())
	assertions.Equal(http.StatusUnauthorized, response.StatusCode)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, Context: t.Context()})
	requirements.NoError(err)
	opts := mcpserver.ServeOptions{Engine: engine, SavedViews: client}
	handler, closeHandler := mcpserver.NewHTTPHandler(opts, mcpserver.HTTPOptions{AllowWrites: true})
	defer closeHandler()
	listener := httptest.NewServer(handler)
	defer listener.Close()
	sdk := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "mailbox-fixture", Version: "1"}, nil)
	session, err := sdk.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: listener.URL}, nil)
	requirements.NoError(err)
	defer func() { _ = session.Close() }()
	state := map[string]any{"filters": []map[string]any{
		{"field": "inbox_state", "operator": "eq", "values": []string{"inbox"}},
		{"field": "read_state", "operator": "in", "values": []string{"unread"}},
	}}
	created, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolCreateSavedView, Arguments: map[string]any{"name": "Inbox Unread", "canonical_state": state, "schema_version": 2}})
	requirements.NoError(err)
	requirements.False(created.IsError, "%+v", created.Content)
	wire, err := json.Marshal(created.StructuredContent)
	requirements.NoError(err)
	var view struct {
		ID       int64 `json:"id"`
		Revision int64 `json:"revision"`
	}
	requirements.NoError(json.Unmarshal(wire, &view))
	requirements.Positive(view.ID)
	ran, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolRunSavedView, Arguments: map[string]any{"id": view.ID}})
	requirements.NoError(err)
	requirements.False(ran.IsError, "%+v", ran.Content)
	wire, err = json.Marshal(ran.StructuredContent)
	requirements.NoError(err)
	var page struct {
		Rows []query.EntryRow `json:"rows"`
	}
	requirements.NoError(json.Unmarshal(wire, &page))
	requirements.Len(page.Rows, 3)
	providers := []string{}
	for _, row := range page.Rows {
		assertions.Equal("inbox", row.InboxState)
		assertions.Equal("unread", row.ReadState)
		assertions.Equal(row.SourceType+"@example.test", row.SourceIdentifier)
		providers = append(providers, row.SourceType)
	}
	assertions.ElementsMatch([]string{"gmail", "imap", "msmail"}, providers)
	invalid, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolUpdateSavedView, Arguments: map[string]any{"id": view.ID, "revision": view.Revision, "canonical_state": map[string]any{"filters": []map[string]any{{"field": "read_state", "operator": "eq", "values": []string{"local_read"}}}}}})
	requirements.NoError(err)
	assertions.True(invalid.IsError)
	unchanged, err := client.GetSavedView(t.Context(), view.ID)
	requirements.NoError(err)
	assertions.Equal(view.Revision, unchanged.Revision)
	readHandler, closeRead := mcpserver.NewHTTPHandler(opts, mcpserver.HTTPOptions{})
	defer closeRead()
	readListener := httptest.NewServer(readHandler)
	defer readListener.Close()
	readSession, err := sdk.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: readListener.URL}, nil)
	requirements.NoError(err)
	defer func() { _ = readSession.Close() }()
	tools, err := readSession.ListTools(t.Context(), nil)
	requirements.NoError(err)
	for _, tool := range tools.Tools {
		assertions.NotEqual(mcpserver.ToolCreateSavedView, tool.Name)
	}
}
