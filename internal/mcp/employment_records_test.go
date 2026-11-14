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
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyEmployment}
	listed := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assert.Contains(listed, name)
	}
	for name, destructive := range map[string]bool{"create_employment": false, "replace_employment": true, "delete_employment": true} {
		annotations, ok := listed[name]["annotations"].(map[string]any)
		require.True(ok)
		assert.Equal(destructive, annotations["destructiveHint"])
	}
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
	bad := map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user", "is_primary": false, "other": true}}
	require.Error(resolved.Validate(bad))
	missingPrimary := map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user"}}
	require.Error(resolved.Validate(missingPrimary))
}
