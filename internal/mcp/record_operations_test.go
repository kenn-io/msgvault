package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordCatalogKeepsSensitiveIntentAndSeparateWriteGate(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	reads := []string{"get_person_record", "list_person_record_history", "list_person_attributes", "list_attribute_definitions", "get_attribute_definition", "get_person_record_media"}
	writes := []string{"update_person_record", "set_person_attribute", "remove_person_attribute", "create_attribute_definition", "update_attribute_definition", "remove_attribute_definition"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: append(reads, writes...)}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range reads {
		assertions.Contains(tools, name)
	}
	for _, name := range writes {
		assertions.NotContains(tools, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRecords}
	tools = toolsByName(t, rawListTools(t, opts, true))
	for _, name := range writes {
		requirements.Contains(tools, name)
	}
	for _, name := range []string{"get_person_record", "list_person_record_history", "list_person_attributes"} {
		schema, ok := tools[name]["inputSchema"].(map[string]any)
		requirements.True(ok)
		assertions.Equal(false, schema["additionalProperties"])
		properties, ok := schema["properties"].(map[string]any)
		requirements.True(ok)
		assertions.Contains(properties, "include_sensitive")
		assertions.Contains(properties, "fields")
	}
	tools = toolsByName(t, rawListTools(t, opts, false))
	for _, name := range writes {
		assertions.NotContains(tools, name)
	}
	opts.DelegatedOnly = true
	assertions.Empty(operationalCatalog(opts, true))
}
