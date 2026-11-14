package mcp

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmploymentCatalogGatesAndClosedInput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	names := []string{"get_employment", "list_person_employments", "list_organization_employments", "create_employment", "replace_employment", "end_employment", "delete_employment"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	reads := toolsByName(t, rawListTools(t, opts, true))
	assert.Contains(reads, "get_employment")
	assert.NotContains(reads, "create_employment")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRecords}
	assert.NotContains(toolsByName(t, rawListTools(t, opts, true)), "create_employment")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyEmployment}
	listed := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assert.Contains(listed, name)
	}
	for _, name := range []string{"list_person_employments", "list_organization_employments"} {
		input, ok := listed[name]["inputSchema"].(map[string]any)
		require.True(ok)
		properties, ok := input["properties"].(map[string]any)
		require.True(ok)
		limit, ok := properties["limit"].(map[string]any)
		require.True(ok)
		description, ok := limit["description"].(string)
		require.True(ok)
		assert.Contains(description, "operation_response_limit_exceeded")
		assert.Contains(description, "offset")
	}
	for name, destructive := range map[string]bool{"create_employment": false, "replace_employment": true, "delete_employment": true} {
		annotations, ok := listed[name]["annotations"].(map[string]any)
		require.True(ok)
		assert.Equal(destructive, annotations["destructiveHint"])
	}
	assert.NotContains(toolsByName(t, rawListTools(t, opts, false)), "create_employment")
	input, ok := listed["create_employment"]["inputSchema"].(map[string]any)
	require.True(ok)
	data, err := json.Marshal(input)
	require.NoError(err)
	var schema jsonschema.Schema
	require.NoError(json.Unmarshal(data, &schema))
	resolved, err := schema.Resolve(nil)
	require.NoError(err)
	good := map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user", "is_primary": false, "title": nil}}
	assert.NoError(resolved.Validate(good))
	bad := map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user", "other": true}}
	require.Error(resolved.Validate(bad))
	assert.NoError(resolved.Validate(map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "extraction", "is_primary": false, "confidence": 2}}))
	assert.NoError(resolved.Validate(map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "extraction", "is_primary": false, "confidence": nil}}))
	require.Error(resolved.Validate(map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "extraction", "confidence": "high"}}))
	body := schema.Properties["employment"]
	confidence := body.Properties["confidence"]
	require.Len(confidence.AnyOf, 2)
	assert.Equal("number", confidence.AnyOf[0].Type)
	assert.Equal("null", confidence.AnyOf[1].Type)
	assert.NotContains(body.Required, "confidence")
	assert.Nil(confidence.AnyOf[0].Minimum)
	assert.Nil(confidence.AnyOf[0].Maximum)
	assert.Nil(confidence.AnyOf[0].Default)
	opts.DelegatedOnly = true
	assert.Empty(operationalCatalog(opts, true))
	found := false
	for _, summary := range ToolSummaries() {
		if summary.Name == "create_employment" {
			found = true
			assert.Equal("--allow-employment-writes", summary.Flag)
			assert.Equal("owner-only", summary.GrantPermission)
		}
	}
	require.True(found)
}
