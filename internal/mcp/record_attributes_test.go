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
	assert := assert.New(t)
	require := require.New(t)
	reads := []string{"list_person_attributes", "list_attribute_definitions", "get_attribute_definition"}
	writes := []string{"set_person_attribute", "create_attribute_definition", "patch_attribute_definition", "clear_person_attribute", "delete_attribute_definition"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: append(slices.Clone(reads), writes...), OperationWriteFamilies: []OperationFamily{OperationFamilyRecords}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range opts.OperationCapabilities {
		assert.Equal(slices.Contains(reads, name), tools[name] != nil, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyAttributes}
	tools = toolsByName(t, rawListTools(t, opts, true))
	data, err := os.ReadFile("../../api/openapi.yaml")
	require.NoError(err)
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	require.NoError(yaml.Unmarshal(data, &doc))
	for _, tc := range []struct {
		tool, component, nested string
		extra                   []string
	}{
		{"set_person_attribute", "SetPersonAttributeRequest", "", []string{"dry_run", "person_id", "slug"}},
		{"create_attribute_definition", "CreateAttributeDefinitionRequest", "definition", nil},
		{"patch_attribute_definition", "PatchAttributeDefinitionRequest", "changes", nil},
	} {
		tool := tools[tc.tool]
		require.NotNil(tool, tc.tool)
		require.NotNil(tool["outputSchema"])
		if tc.nested != "" {
			schema, ok := tool["inputSchema"].(map[string]any)
			require.True(ok)
			properties, ok := schema["properties"].(map[string]any)
			require.True(ok)
			tool = map[string]any{"inputSchema": properties[tc.nested]}
		}
		properties := toolPropertyNames(t, tool)
		native := tc.extra
		for name := range doc.Components.Schemas[tc.component].Properties {
			if name != "object_type" {
				native = append(native, name)
			}
		}
		assert.ElementsMatch(native, properties)
	}
	patchSchema, ok := tools["patch_attribute_definition"]["inputSchema"].(map[string]any)
	require.True(ok)
	patchProperties, ok := patchSchema["properties"].(map[string]any)
	require.True(ok)
	changes, ok := patchProperties["changes"].(map[string]any)
	require.True(ok)
	changeProperties, ok := changes["properties"].(map[string]any)
	require.True(ok)
	patch, ok := changeProperties["description"].(map[string]any)
	require.True(ok)
	assert.ElementsMatch([]any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}, patch["anyOf"])
	for _, name := range []string{"clear_person_attribute", "patch_attribute_definition", "delete_attribute_definition"} {
		require.NotNil(tools[name], name)
		annotations, ok := tools[name]["annotations"].(map[string]any)
		require.True(ok, name)
		assert.Equal(true, annotations["destructiveHint"], name)
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
		{"clear_person_attribute", map[string]any{"person_id": 1, "slug": "example", "expected_value_id": 0}},
	} {
		result := rawCallTool(t, opts, tc.name, tc.args)
		assert.Equal(t, true, result["isError"], tc.name)
	}
	assert.Zero(t, spy.disclosures.Load())
	assert.Zero(t, spy.operations.Load())
}
