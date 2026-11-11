package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
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
	assert.Equal(t, OperationFamilySources, found.family)
	assert.False(t, found.writes)
	assert.Equal(t, operationalOutputSchema(outputSchemaFor[generated.ChangesResponse]()), found.definition.outputSchema)
	for _, field := range []string{"cursor", "limit", "person_id", "source_ids", "conversation_ids", "incoming_only"} {
		assert.Contains(t, found.definition.inputSchema.Properties, field)
	}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_changes"}, DelegatedOnly: true}
	assert.Empty(t, operationalCatalog(opts, false))
}
