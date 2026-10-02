package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBriefSweepCatalogSeparatesCuratedWritesFromInferenceRuns(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_person_brief_versions", "get_person_brief_enrollment", "set_person_brief_enrollment", "reject_person_brief", "generate_person_brief", "get_people_sweep_status", "list_people_sweep_history", "run_people_sweep"}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range []string{"list_person_brief_versions", "get_person_brief_enrollment", "get_people_sweep_status", "list_people_sweep_history"} {
		assertions.Contains(tools, name)
	}
	for _, name := range []string{"set_person_brief_enrollment", "reject_person_brief", "generate_person_brief", "run_people_sweep"} {
		assertions.NotContains(tools, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRecords}
	tools = toolsByName(t, rawListTools(t, opts, true))
	requirements.Contains(tools, "set_person_brief_enrollment")
	requirements.Contains(tools, "reject_person_brief")
	assertions.NotContains(tools, "generate_person_brief")
	assertions.NotContains(tools, "run_people_sweep")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyInference}
	tools = toolsByName(t, rawListTools(t, opts, true))
	requirements.Contains(tools, "generate_person_brief")
	requirements.Contains(tools, "run_people_sweep")
	assertions.NotContains(tools, "set_person_brief_enrollment")
	assertions.NotContains(tools, "reject_person_brief")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyInference, OperationFamilyRecords}
	tools = toolsByName(t, rawListTools(t, opts, true))
	for _, name := range opts.OperationCapabilities {
		schema, ok := tools[name]["inputSchema"].(map[string]any)
		requirements.True(ok)
		assertions.Equal(false, schema["additionalProperties"], name)
		if schema["properties"] == nil {
			requirements.Equal("get_people_sweep_status", name, "the only zero-argument tool omits the empty properties object")
			continue
		}
		properties, ok := schema["properties"].(map[string]any)
		requirements.True(ok)
		for _, forbidden := range []string{"etag", "version", "env", "path", "command", "endpoint", "provider", "credential", "allow_sensitive"} {
			assertions.NotContains(properties, forbidden, name)
		}
	}
	enrollment, ok := tools["set_person_brief_enrollment"]["inputSchema"].(map[string]any)
	requirements.True(ok)
	assertions.Contains(enrollment["required"], "enrolled", "false is a required value, not an absent argument")
	tools = toolsByName(t, rawListTools(t, opts, false))
	assertions.NotContains(tools, "generate_person_brief")
	assertions.NotContains(tools, "run_people_sweep")
	opts.DelegatedOnly = true
	assertions.Empty(operationalCatalog(opts, true))
}
