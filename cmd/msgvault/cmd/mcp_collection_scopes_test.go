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
	members := apiprotocol.MCPRouteDescriptor{OperationID: "getCLICollectionMembers", Method: "GET", Path: "/api/v1/cli/collection/members", QueryParameters: []string{"name"}}
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route}}))
	assert.True(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route, members}}))
	members.QueryParameters = nil
	assert.False(mcpCollectionScopesAvailable(&apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{route, members}}))
}
