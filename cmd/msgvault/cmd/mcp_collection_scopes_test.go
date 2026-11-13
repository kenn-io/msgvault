package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/apiprotocol"
)

func TestCollectionScopeDiscoveryRequiresExactRoute(t *testing.T) {
	assert := assert.New(t)
	assert.False(mcpCollectionScopesAvailable(nil))
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{}))
	route := apiprotocol.MCPRouteDescriptor{OperationID: "listCLICollections", Method: "GET", Path: "/api/v1/cli/collections"}
	assert.True(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
	route.Method = "POST"
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
	route.Method = "GET"
	route.OperationID = "other"
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
	route.OperationID = "listCLICollections"
	route.Path = "/api/v1/cli/other"
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
}
