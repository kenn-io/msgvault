package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOrganizationReadCatalogIsOwnerOnlyAndCapabilityGated(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_organization", "get_organization_history", "list_organization_attributes"}}
	tools := toolsByName(t, rawListTools(t, opts, false))
	assert.Contains(t, tools, "get_organization")
	assert.Contains(t, tools, "get_organization_history")
	assert.Contains(t, tools, "list_organization_attributes")
	assert.NotContains(t, tools, "list_organizations")
	opts.OperationCapabilities = nil
	assert.Empty(t, rawListTools(t, opts, false))
	opts.OperationCapabilities = []string{"get_organization"}
	opts.DelegatedOnly = true
	assert.Empty(t, rawListTools(t, opts, true))
}

func TestOrganizationWritesNeedSeparateOptIn(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_organization", "delete_organization"}, OperationWriteFamilies: []OperationFamily{OperationFamilyRecords}}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), "delete_organization")
	opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, OperationFamilyOrganizations)
	assert.Contains(t, toolsByName(t, rawListTools(t, opts, true)), "delete_organization")
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, false)), "delete_organization")
	opts.DelegatedOnly = true
	assert.Empty(t, rawListTools(t, opts, true))
}
