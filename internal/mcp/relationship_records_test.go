package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRelationshipReadCatalogIsOwnerOnly(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_relationship_types", "get_person_network"}}
	tools := toolsByName(t, rawListTools(t, opts, false))
	assert.Contains(t, tools, "list_relationship_types")
	assert.Contains(t, tools, "get_person_network")
	opts.DelegatedOnly = true
	assert.Empty(t, rawListTools(t, opts, false))
}
