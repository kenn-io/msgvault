package mcp

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRelationshipCatalogPreservesReadAndWriteBoundaries(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	reads := []string{"list_organizations", "get_organization", "get_organization_history", "list_organization_attributes", "get_organization_record_media", "get_employment", "list_person_employments", "list_organization_employments", "list_relationship_types", "get_relationship_type", "list_person_relationships", "get_person_relationship_record", "list_person_relationship_reviews", "get_person_network"}
	writes := []string{"create_organization", "update_organization", "remove_organization", "merge_organizations", "update_organization_profile", "set_organization_attribute", "remove_organization_attribute", "create_employment", "update_employment", "remove_employment", "end_employment", "set_primary_employment", "create_relationship_type", "update_relationship_type", "remove_relationship_type", "create_person_relationship", "update_person_relationship", "remove_person_relationship"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: append(reads, writes...)}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range reads {
		_, present := tools[name]
		assertions.True(present, "missing read: %s", name)
	}
	for _, name := range writes {
		_, present := tools[name]
		assertions.False(present, "unexpected default write: %s", name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRecords}
	tools = toolsByName(t, rawListTools(t, opts, true))
	for _, name := range writes {
		_, present := tools[name]
		requirements.True(present, "missing explicitly enabled write: %s", name)
	}
	for _, name := range append(reads, writes...) {
		input, ok := tools[name]["inputSchema"].(map[string]any)
		requirements.True(ok)
		assertions.Equal(false, input["additionalProperties"], "closed input for %s", name)
	}
	tools = toolsByName(t, rawListTools(t, opts, false))
	for _, name := range writes {
		_, present := tools[name]
		assertions.False(present, "HTTP write gate for %s", name)
	}
	opts.DelegatedOnly = true
	assertions.Empty(operationalCatalog(opts, true))
}
