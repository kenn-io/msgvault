package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListChangesDefinition(t *testing.T) {
	var found *operationalDefinition
	for _, definition := range sourceOperationalDefinitions() {
		if definition.definition.name == "list_changes" {
			found = &definition
			break
		}
	}
	require.NotNil(t, found)
	for _, field := range []string{"cursor", "limit", "person_id", "source_ids", "conversation_ids", "incoming_only"} {
		assert.Contains(t, found.definition.inputSchema.Properties, field)
	}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_changes"}, DelegatedOnly: true}
	assert.Empty(t, operationalCatalog(opts, false))
	opts.GrantPermissions = []string{"search.read"}
	require.Len(t, operationalCatalog(opts, false), 1)
	assert.Equal(t, "list_changes", operationalCatalog(opts, false)[0].definition.name)
	opts.OperationCapabilities = nil
	assert.Empty(t, operationalCatalog(opts, false))
}
