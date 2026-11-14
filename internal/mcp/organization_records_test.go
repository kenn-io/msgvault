package mcp

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrganizationWritesNeedSeparateOptIn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_organization", "create_organization", "update_organization", "delete_organization", "replace_organization_profile", "set_organization_attribute"}, OperationWriteFamilies: []OperationFamily{OperationFamilyRecords}}
	assert.NotContains(toolsByName(t, rawListTools(t, opts, true)), "delete_organization")
	opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, OperationFamilyOrganizations)
	listed := toolsByName(t, rawListTools(t, opts, true))
	assert.Contains(listed, "delete_organization")
	for name, destructive := range map[string]bool{"create_organization": false, "set_organization_attribute": true, "update_organization": true, "delete_organization": true, "replace_organization_profile": true} {
		annotations, ok := listed[name]["annotations"].(map[string]any)
		require.True(ok)
		assert.Equal(destructive, annotations["destructiveHint"], name)
	}
	input, ok := listed["create_organization"]["inputSchema"].(map[string]any)
	require.True(ok)
	data, err := json.Marshal(input)
	require.NoError(err)
	var schema jsonschema.Schema
	require.NoError(json.Unmarshal(data, &schema))
	resolved, err := schema.Resolve(nil)
	require.NoError(err)
	assert.NoError(resolved.Validate(map[string]any{"body": map[string]any{"name": "Example"}}))
	require.Error(resolved.Validate(map[string]any{"body": map[string]any{"name": "Example", "unexpected": "value"}}))
}
