package mcp

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelationshipReadCatalogIsOwnerOnly(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_relationship_types", "get_person_network"}}
	tools := toolsByName(t, rawListTools(t, opts, false))
	assert.Contains(t, tools, "list_relationship_types")
	assert.Contains(t, tools, "get_person_network")
	opts.DelegatedOnly = true
	assert.Empty(t, rawListTools(t, opts, false))
}

func TestRelationshipWritesHaveSeparateOptIn(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_relationship_types", "create_relationship_type"}}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), "create_relationship_type")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRelationshipRecords}
	assert.Contains(t, toolsByName(t, rawListTools(t, opts, true)), "create_relationship_type")
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, false)), "create_relationship_type")
}

func TestRelationshipSDKRejectsUnknownWriteFields(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"update_person_relationship"}, OperationWriteFamilies: []OperationFamily{OperationFamilyRelationshipRecords}}
	session := task5ConnectClient(t, opts, true)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_person_relationship", Arguments: map[string]any{"id": 1, "etag": "tag", "notes": nil, "unexpected": true}})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}
