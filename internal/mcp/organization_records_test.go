package mcp

import (
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
)

func TestOrganizationWritesNeedSeparateOptIn(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_organization", "delete_organization"}, OperationWriteFamilies: []OperationFamily{OperationFamilyRecords}}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), "delete_organization")
	opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, OperationFamilyOrganizations)
	assert.Contains(t, toolsByName(t, rawListTools(t, opts, true)), "delete_organization")
	assert.Equal(t, &jsonschema.Schema{}, organizationSetAttributeSchema().Properties["body"].Properties["value"].Properties["json"])
	assert.Equal(t, &jsonschema.Schema{}, organizationAttributesSchema().Properties["values"].Items.Properties["value"].Properties["json"])
	write := organizationAttributeWriteSchema()
	assert.Equal(t, &jsonschema.Schema{}, write.Properties["value"].Properties["value"].Properties["json"])
	assert.Equal(t, &jsonschema.Schema{}, write.Properties["superseded"].Properties["value"].Properties["json"])
}
