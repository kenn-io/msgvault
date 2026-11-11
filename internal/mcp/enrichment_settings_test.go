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
	schema, err := definitions[1].definition.inputSchema.Resolve(nil)
	require.NoError(err)
	assert.NoError(schema.Validate(map[string]any{"name": "example", "etag": `"revision"`, "enabled": false, "max_requests_per_day": float64(0)}))
	for _, field := range []struct {
		name  string
		value any
	}{{"endpoint", "example"}, {"poll_endpoint", "example"}, {"credential", "example"}, {"target_keys", []any{"attribute:bio"}}, {"allowed_identifiers", []any{"name"}}, {"kind", "example"}} {
		require.Error(schema.Validate(map[string]any{"name": "example", "etag": `"revision"`, field.name: field.value}), field.name)
	}
	require.Error(schema.Validate(map[string]any{"name": "example", "etag": `"revision"`, "max_retries": float64(-1)}))
	require.Error(schema.Validate(map[string]any{"name": "example", "etag": `"revision"`, "max_retries": float64(9007199254740992)}))
}
