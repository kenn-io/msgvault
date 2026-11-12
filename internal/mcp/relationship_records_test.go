package mcp

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelationshipWritesHaveSeparateOptIn(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_relationship_types", "get_person_network", "create_relationship_type"}}
	reads := toolsByName(t, rawListTools(t, opts, false))
	assert.Contains(t, reads, "list_relationship_types")
	assert.Contains(t, reads, "get_person_network")
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), "create_relationship_type")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRelationshipRecords}
	assert.Contains(t, toolsByName(t, rawListTools(t, opts, true)), "create_relationship_type")
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, false)), "create_relationship_type")
}

func TestRelationshipSDKRejectsUnknownWriteFields(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"update_person_relationship", "update_relationship_type", "get_person_network", "list_person_relationship_reviews"}, OperationWriteFamilies: []OperationFamily{OperationFamilyRelationshipRecords}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range opts.OperationCapabilities {
		schema, ok := tools[name]["inputSchema"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, false, schema["additionalProperties"])
	}
	session := task5ConnectClient(t, opts, true)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"update_person_relationship", map[string]any{"id": 1, "etag": "tag", "unexpected": true}},
		{"update_person_relationship", map[string]any{"id": 1, "etag": "tag", "end_date": nil}},
		{"update_relationship_type", map[string]any{"id": 1, "etag": "tag", "description": nil}},
		{"update_person_relationship", map[string]any{"id": 1, "etag": "tag", "end_date": ""}},
		{"get_person_network", map[string]any{"person_id": 1, "depth": 4}},
		{"list_person_relationship_reviews", map[string]any{"status": "x"}},
	} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		require.NoError(t, err)
		require.True(t, result.IsError)
		assert.Contains(t, task5SDKResultText(result), `validating "arguments"`)
	}
}
