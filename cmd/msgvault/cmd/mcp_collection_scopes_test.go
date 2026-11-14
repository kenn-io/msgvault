package cmd

import (
	"log/slog"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCollectionScopeDiscoveryRequiresExactRoute(t *testing.T) {
	assert := assert.New(t)
	assert.False(mcpCollectionScopesAvailable(nil))
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{}))
	route := apiprotocol.MCPRouteDescriptor{OperationID: "listCLICollections", Method: "GET", Path: "/api/v1/cli/collections"}
	members := apiprotocol.MCPRouteDescriptor{OperationID: "getCLICollectionMembers", Method: "GET", Path: "/api/v1/cli/collection/members", QueryParameters: []string{"name"}}
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
	assert.True(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route, members}}))
	members.QueryParameters = nil
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route, members}}))
	members.QueryParameters = []string{"name"}
	route.Method = "POST"
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route, members}}))
	route.Method = "GET"
	route.OperationID = "other"
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route, members}}))
	route.OperationID = "listCLICollections"
	route.Path = "/api/v1/cli/other"
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route, members}}))
}

func TestMCPEmptyCollectionSemanticSearchReportsUnavailable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "empty@example.test")
	require.NoError(err)
	_, err = st.CreateCollection("empty", "", []int64{source.ID})
	require.NoError(err)
	require.NoError(st.RemoveSourcesFromCollection("empty", []int64{source.ID}))
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	session := operationMCPSession(t, backend, nil, nil)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "semantic_search_messages", Arguments: map[string]any{"query": "fixture", "collection": "empty"}})
	require.NoError(err)
	require.True(result.IsError)
	require.NotEmpty(result.Content)
	msg, ok := result.Content[0].(*sdkmcp.TextContent)
	require.True(ok)
	assert.Contains(msg.Text, "vector_not_enabled")
}
