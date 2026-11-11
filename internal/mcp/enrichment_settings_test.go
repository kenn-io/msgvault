package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnrichmentSettingsInputSchemaRejectsProviderSetup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	definitions := enrichmentSettingsDefinitions()
	require.Len(definitions, 2)
	require.NotNil(definitions[1].definition.annotations.OpenWorldHint)
	require.NotNil(definitions[1].definition.annotations.DestructiveHint)
	assert.False(*definitions[1].definition.annotations.OpenWorldHint)
	assert.True(*definitions[1].definition.annotations.DestructiveHint)
	schema, err := definitions[1].definition.inputSchema.Resolve(nil)
	require.NoError(err)
	assert.NoError(schema.Validate(map[string]any{"name": "example", "etag": `"revision"`, "enabled": false, "max_requests_per_day": float64(0)}))
	for _, field := range []string{"endpoint", "poll_endpoint", "credential", "target_keys", "allowed_identifiers", "kind"} {
		assert.Error(schema.Validate(map[string]any{"name": "example", "etag": `"revision"`, field: "example"}), field)
	}
	assert.Error(schema.Validate(map[string]any{"name": "example", "etag": `"revision"`, "max_retries": float64(-1)}))
	assert.Error(schema.Validate(map[string]any{"name": "example", "etag": `"revision"`, "max_retries": float64(9007199254740992)}))
}
