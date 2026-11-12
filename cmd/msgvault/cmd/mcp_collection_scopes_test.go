package cmd

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCollectionScopeDiscoveryRequiresExactRoute(t *testing.T) {
	assert.False(t, mcpCollectionScopesAvailable(nil))
	assert.False(t, mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{}))
	route := apiprotocol.MCPRouteDescriptor{OperationID: "listCLICollections", Method: "GET", Path: "/api/v1/cli/collections"}
	assert.True(t, mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
	route.Method = "POST"
	assert.False(t, mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
}

func TestDaemonHybridCollectionSourceReachesRouter(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, _, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "semantic@example.test", "fixture", "fixture", "fixture")
	require.NoError(t, err)
	_, err = st.CreateCollection("one", "", []int64{source.ID})
	require.NoError(t, err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner"}}
	cfg.Data.DataDir = t.TempDir()
	router := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)}).Router()
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/search" {
			seen = r.URL.Query().Get("source_id")
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- mcpserver.ServeTransport(ctx, mcpserver.ServeOptions{Engine: daemonclient.NewEngineAdapter(client), HybridSearcher: daemonMCPHybridSearcher{client: client}, CollectionScopesAvailable: true}, serverTransport)
	}()
	sdk := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "collection-test", Version: "1"}, nil)
	session, err := sdk.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: mcpserver.ToolSemanticSearchMessages, Arguments: map[string]any{"query": "fixture", "collection": "one"}})
	require.NoError(t, err)
	assert.True(t, result.IsError) // The real daemon has no vector backend in this fixture.
	assert.Equal(t, strconv.FormatInt(source.ID, 10), seen)
	require.NoError(t, session.Close())
	cancel()
	if err := <-done; err != nil {
		require.ErrorIs(t, err, context.Canceled)
	}
}
