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
	assert.Error(t, resolved.Validate(bad))
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
