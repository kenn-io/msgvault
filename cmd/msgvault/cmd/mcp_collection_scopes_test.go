package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/apiprotocol"
)

func TestCollectionScopeDiscoveryRequiresExactRoute(t *testing.T) {
	assert.False(t, mcpCollectionScopesAvailable(nil))
	assert.False(t, mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{}))
	route := apiprotocol.MCPRouteDescriptor{OperationID: "listCLICollections", Method: "GET", Path: "/api/v1/cli/collections"}
	assert.True(t, mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
	route.Method = "POST"
	assert.False(t, mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
	route.Method = "GET"
	route.OperationID = "other"
	assert.False(t, mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
	route.OperationID = "listCLICollections"
	route.Path = "/api/v1/cli/other"
	assert.False(t, mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
}
