package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOrganizationWritesNeedSeparateOptIn(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_organization", "delete_organization"}, OperationWriteFamilies: []OperationFamily{OperationFamilyRecords}}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), "delete_organization")
	opts.OperationWriteFamilies = append(opts.OperationWriteFamilies, OperationFamilyOrganizations)
	assert.Contains(t, toolsByName(t, rawListTools(t, opts, true)), "delete_organization")
}
