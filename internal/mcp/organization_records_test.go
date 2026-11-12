package mcp

import (
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrganizationWritesNeedSeparateOptIn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_organization", "create_organization", "update_organization", "delete_organization", "merge_organization", "replace_organization_profile", "set_organization_attribute", "clear_organization_attribute"}, OperationWriteFamilies: []OperationFamily{OperationFamilyRecords}}
	assert.NotContains(toolsByName(t, rawListTools(t, opts, true)), "delete_organization")
	opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, OperationFamilyOrganizations)
	listed := toolsByName(t, rawListTools(t, opts, true))
	assert.Contains(listed, "delete_organization")
	for name, destructive := range map[string]bool{"create_organization": false, "set_organization_attribute": true, "update_organization": true, "delete_organization": true, "merge_organization": true, "replace_organization_profile": true, "clear_organization_attribute": true} {
		annotations, ok := listed[name]["annotations"].(map[string]any)
		require.True(ok)
		assert.Equal(destructive, annotations["destructiveHint"], name)
	}
	assert.Equal(&jsonschema.Schema{}, organizationSetAttributeSchema().Properties["body"].Properties["value"].Properties["json"])
	assert.Equal(&jsonschema.Schema{}, organizationAttributesSchema().Properties["values"].Items.Properties["value"].Properties["json"])
	write := organizationAttributeWriteSchema()
	assert.Equal(&jsonschema.Schema{}, write.Properties["value"].Properties["value"].Properties["json"])
	assert.Equal(&jsonschema.Schema{}, write.Properties["superseded"].Properties["value"].Properties["json"])
}
