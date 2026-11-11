package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttributeCatalogAvailabilityAndIndependentWriteGate(t *testing.T) {
	names := []string{"list_person_attributes", "set_person_attribute", "clear_person_attribute", "list_attribute_definitions", "get_attribute_definition", "create_attribute_definition", "patch_attribute_definition", "delete_attribute_definition"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	read := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range []string{"list_person_attributes", "list_attribute_definitions", "get_attribute_definition"} {
		assert.Contains(t, read, name)
	}
	for _, name := range []string{"set_person_attribute", "clear_person_attribute", "create_attribute_definition", "patch_attribute_definition", "delete_attribute_definition"} {
		assert.NotContains(t, read, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRecords}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), "set_person_attribute")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyAttributes}
	all := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assert.Contains(t, all, name)
	}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, false)), "set_person_attribute")
	opts.OperationCapabilities = []string{"list_person_attributes"}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), "set_person_attribute")
	opts.DelegatedOnly = true
	assert.Empty(t, rawListTools(t, opts, true))
}

func TestAttributeCatalogUsesClosedInputSchemas(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"set_person_attribute", "create_attribute_definition", "patch_attribute_definition"}, OperationWriteFamilies: []OperationFamily{OperationFamilyAttributes}}
	defs := operationalCatalog(opts, true)
	require.Len(t, defs, 3)
	for _, d := range defs {
		assert.NotNil(t, d.definition.inputSchema.AdditionalProperties)
		assert.NotNil(t, d.definition.inputSchema.AdditionalProperties.Not)
		require.NotNil(t, d.definition.annotations.ReadOnlyHint)
		assert.False(t, *d.definition.annotations.ReadOnlyHint)
	}
	for _, d := range defs {
		switch d.definition.name {
		case "set_person_attribute":
			assert.NotNil(t, d.definition.inputSchema.Properties["value"].AdditionalProperties.Not)
		case "create_attribute_definition":
			assert.NotNil(t, d.definition.inputSchema.Properties["definition"].AdditionalProperties.Not)
		case "patch_attribute_definition":
			assert.NotNil(t, d.definition.inputSchema.Properties["changes"].AdditionalProperties.Not)
		}
	}
}
