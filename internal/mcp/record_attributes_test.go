package mcp

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type attributeValidationSpy struct {
	disclosures atomic.Int32
	operations  atomic.Int32
}

func (s *attributeValidationSpy) OperationDisclosure(context.Context, string, map[string]any) (string, error) {
	s.disclosures.Add(1)
	return "", nil
}

func (s *attributeValidationSpy) ExecuteOperation(context.Context, string, map[string]any) (*OperationResult, error) {
	s.operations.Add(1)
	return &OperationResult{}, nil
}

func TestAttributeToolContract(t *testing.T) {
	reads := []string{"list_person_attributes", "list_attribute_definitions", "get_attribute_definition"}
	writes := []string{"set_person_attribute", "create_attribute_definition", "patch_attribute_definition", "clear_person_attribute", "delete_attribute_definition"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: append(slices.Clone(reads), writes...), OperationWriteFamilies: []OperationFamily{OperationFamilyRecords}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range opts.OperationCapabilities {
		assert.Equal(t, slices.Contains(reads, name), tools[name] != nil, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyAttributes}
	tools = toolsByName(t, rawListTools(t, opts, true))
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
	patch := tools["patch_attribute_definition"]["inputSchema"].(map[string]any)["properties"].(map[string]any)["changes"].(map[string]any)["properties"].(map[string]any)["description"].(map[string]any)
	assert.ElementsMatch(t, []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}, patch["anyOf"])
	for _, name := range []string{"clear_person_attribute", "patch_attribute_definition", "delete_attribute_definition"} {
		require.NotNil(t, tools[name], name)
		assert.Equal(t, true, tools[name]["annotations"].(map[string]any)["destructiveHint"], name)
	}
}

func TestAttributeToolRejectsArgumentsOutsideSchema(t *testing.T) {
	spy := &attributeValidationSpy{}
	opts := ServeOptions{Operations: spy, OperationCapabilities: []string{"create_attribute_definition", "list_person_attributes", "clear_person_attribute"}, OperationWriteFamilies: []OperationFamily{OperationFamilyAttributes}}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"create_attribute_definition", map[string]any{"definition": map[string]any{"object_type": "organization", "slug": "test", "label": "Test", "value_type": "text", "field_type": "text", "cardinality": "single"}}},
		{"list_person_attributes", map[string]any{"person_id": "1"}},
		{"list_person_attributes", map[string]any{"person_id": 1, "unexpected": true}},
		{"list_person_attributes", map[string]any{"person_id": json.Number("1.0000000000000001")}},
		{"list_person_attributes", map[string]any{"person_id": 0}},
		{"list_person_attributes", map[string]any{"person_id": json.Number("9007199254740992")}},
		{"clear_person_attribute", map[string]any{"person_id": 1, "slug": "example", "ordinal": -1}},
		{"clear_person_attribute", map[string]any{"person_id": 1, "slug": "example", "ordinal": json.Number("1.0000000000000001")}},
		{"clear_person_attribute", map[string]any{"person_id": 1, "slug": "example", "expected_value_id": 0}},
	} {
		result := rawCallTool(t, opts, tc.name, tc.args)
		assert.Equal(t, true, result["isError"], tc.name)
	}
	assert.Zero(t, spy.disclosures.Load())
	assert.Zero(t, spy.operations.Load())
}
