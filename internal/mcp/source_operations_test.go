package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListChangesDefinition(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var found *operationalDefinition
	for _, definition := range sourceOperationalDefinitions() {
		if definition.definition.name == "list_changes" {
			found = &definition
			break
		}
	}
	requirements.NotNil(found)
	for _, field := range []string{"cursor", "limit", "person_id", "source_ids", "conversation_ids", "incoming_only"} {
		assertions.Contains(found.definition.inputSchema.Properties, field)
	}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_changes"}, DelegatedOnly: true}
	assertions.Empty(operationalCatalog(opts, false))
	opts.GrantPermissions = []string{"search.read"}
	requirements.Len(operationalCatalog(opts, false), 1)
	assertions.Equal("list_changes", operationalCatalog(opts, false)[0].definition.name)
	opts.OperationCapabilities = nil
	assertions.Empty(operationalCatalog(opts, false))
}
