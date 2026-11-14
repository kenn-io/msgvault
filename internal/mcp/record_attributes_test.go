package mcp

import (
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestAttributeCatalogAvailabilityAndIndependentWriteGate(t *testing.T) {
	writes := []string{"set_person_attribute", "clear_person_attribute", "create_attribute_definition", "patch_attribute_definition", "delete_attribute_definition"}
	reads := []string{"list_person_attributes", "list_attribute_definitions", "get_attribute_definition"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: append(slices.Clone(reads), writes...)}
	for _, families := range [][]OperationFamily{nil, {OperationFamilyRecords}, {OperationFamilyAttributes}} {
		opts.OperationWriteFamilies = families
		tools := toolsByName(t, rawListTools(t, opts, true))
		for _, name := range reads {
			assert.Contains(t, tools, name)
		}
		for _, name := range writes {
			if slices.Contains(families, OperationFamilyAttributes) {
				assert.Contains(t, tools, name)
			} else {
				assert.NotContains(t, tools, name)
			}
		}
	}
}

func TestAttributeToolContract(t *testing.T) {
	names := []string{"set_person_attribute", "create_attribute_definition", "patch_attribute_definition", "clear_person_attribute", "delete_attribute_definition"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names, OperationWriteFamilies: []OperationFamily{OperationFamilyAttributes}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	data, err := os.ReadFile("../../api/openapi.yaml")
	require.NoError(t, err)
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	for _, tc := range []struct {
		tool, component, nested string
		expected, extra         []string
	}{
		{"set_person_attribute", "SetPersonAttributeRequest", "", []string{"active_from", "active_until", "actor", "confidence", "dry_run", "expected_empty", "expected_value_id", "ordinal", "person_id", "slug", "source", "source_ref", "value"}, []string{"dry_run", "person_id", "slug"}},
		{"create_attribute_definition", "CreateAttributeDefinitionRequest", "definition", []string{"cardinality", "description", "display_order", "field_type", "is_audited", "is_required", "is_searchable", "is_sensitive", "label", "options", "record_target", "slug", "value_type", "vcard_property"}, nil},
		{"patch_attribute_definition", "PatchAttributeDefinitionRequest", "changes", []string{"description", "display_order", "is_active", "is_sensitive", "label"}, nil},
	} {
		tool := tools[tc.tool]
		require.NotNil(t, tool, tc.tool)
		require.NotNil(t, tool["outputSchema"])
		if tc.nested != "" {
			tool = map[string]any{"inputSchema": tool["inputSchema"].(map[string]any)["properties"].(map[string]any)[tc.nested]}
		}
		properties := toolPropertyNames(t, tool)
		assert.Equal(t, tc.expected, properties)
		native := tc.extra
		for name := range doc.Components.Schemas[tc.component].Properties {
			if name != "object_type" {
				native = append(native, name)
			}
		}
		assert.ElementsMatch(t, native, properties)
	}
	for _, name := range []string{"clear_person_attribute", "patch_attribute_definition", "delete_attribute_definition"} {
		require.NotNil(t, tools[name], name)
		assert.Equal(t, true, tools[name]["annotations"].(map[string]any)["destructiveHint"], name)
	}
}
