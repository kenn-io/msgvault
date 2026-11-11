package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttributeCatalogAvailabilityAndIndependentWriteGate(t *testing.T) {
	assertions := assert.New(t)
	names := []string{"list_person_attributes", "set_person_attribute", "clear_person_attribute", "list_attribute_definitions", "get_attribute_definition", "create_attribute_definition", "patch_attribute_definition", "delete_attribute_definition"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	read := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range []string{"list_person_attributes", "list_attribute_definitions", "get_attribute_definition"} {
		assertions.Contains(read, name)
	}
	for _, name := range []string{"set_person_attribute", "clear_person_attribute", "create_attribute_definition", "patch_attribute_definition", "delete_attribute_definition"} {
		assertions.NotContains(read, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRecords}
	assertions.NotContains(toolsByName(t, rawListTools(t, opts, true)), "set_person_attribute")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyAttributes}
	all := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assertions.Contains(all, name)
	}
	for _, definition := range operationalCatalog(opts, true) {
		name := definition.definition.name
		switch name {
		case "list_person_attributes", "list_attribute_definitions", "get_attribute_definition":
			assertions.True(definition.definition.annotations.ReadOnlyHint)
		default:
			assertions.False(definition.definition.annotations.ReadOnlyHint)
		}
		require.NotNil(t, definition.definition.annotations.DestructiveHint)
		assertions.Equal(name == "patch_attribute_definition" || name == "delete_attribute_definition", *definition.definition.annotations.DestructiveHint)
	}
}
