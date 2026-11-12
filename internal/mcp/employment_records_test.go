package mcp

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmploymentCatalogGatesAndClosedInput(t *testing.T) {
	names := []string{"get_employment", "list_person_employments", "list_organization_employments", "create_employment", "replace_employment", "end_employment", "set_primary_employment", "delete_employment"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	reads := toolsByName(t, rawListTools(t, opts, true))
	assert.Contains(t, reads, "get_employment")
	assert.NotContains(t, reads, "create_employment")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRecords}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), "create_employment")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyEmployment}
	listed := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assert.Contains(t, listed, name)
	}
	for _, name := range []string{"list_person_employments", "list_organization_employments"} {
		input, ok := listed[name]["inputSchema"].(map[string]any)
		require.True(t, ok)
		properties, ok := input["properties"].(map[string]any)
		require.True(t, ok)
		limit, ok := properties["limit"].(map[string]any)
		require.True(t, ok)
		description, ok := limit["description"].(string)
		require.True(t, ok)
		assert.Contains(t, description, "operation_response_limit_exceeded")
		assert.Contains(t, description, "offset")
	}
	for name, destructive := range map[string]bool{"create_employment": false, "replace_employment": true, "delete_employment": true} {
		annotations, ok := listed[name]["annotations"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, destructive, annotations["destructiveHint"])
	}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, false)), "create_employment")
	input, ok := listed["create_employment"]["inputSchema"].(map[string]any)
	require.True(t, ok)
	data, err := json.Marshal(input)
	require.NoError(t, err)
	var schema jsonschema.Schema
	require.NoError(t, json.Unmarshal(data, &schema))
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	good := map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user", "is_primary": false, "title": nil}}
	assert.NoError(t, resolved.Validate(good))
	bad := map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user", "other": true}}
	require.Error(t, resolved.Validate(bad))
	assert.NoError(t, resolved.Validate(map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "extraction", "confidence": 2}}))
	assert.NoError(t, resolved.Validate(map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "extraction", "confidence": nil}}))
	require.Error(t, resolved.Validate(map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "extraction", "confidence": "high"}}))
	body := schema.Properties["employment"]
	confidence := body.Properties["confidence"]
	require.Len(t, confidence.AnyOf, 2)
	assert.Equal(t, "number", confidence.AnyOf[0].Type)
	assert.Equal(t, "null", confidence.AnyOf[1].Type)
	assert.NotContains(t, body.Required, "confidence")
	assert.Nil(t, confidence.AnyOf[0].Minimum)
	assert.Nil(t, confidence.AnyOf[0].Maximum)
	assert.Nil(t, confidence.AnyOf[0].Default)
	opts.DelegatedOnly = true
	assert.Empty(t, operationalCatalog(opts, true))
	found := false
	for _, summary := range ToolSummaries() {
		if summary.Name == "create_employment" {
			found = true
			assert.Equal(t, "--allow-employment-writes", summary.Flag)
			assert.Equal(t, "owner-only", summary.GrantPermission)
		}
	}
	require.True(t, found)
}
