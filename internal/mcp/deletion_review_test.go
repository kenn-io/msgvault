package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeletionReviewCatalogReadOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_deletions", "get_deletion"}}
	definitions := operationalCatalog(opts, false)
	require.Len(definitions, 2)
	for _, definition := range definitions {
		assert.True(definition.definition.annotations.ReadOnlyHint)
	}
	schema, err := definitions[0].definition.inputSchema.Resolve(nil)
	require.NoError(err)
	assert.NoError(schema.Validate(map[string]any{"status": "future_status"}))
	assert.Equal([]string{"batch_id"}, definitions[1].definition.inputSchema.Required)
}
