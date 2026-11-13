package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportJobCatalogGatesAndSchemas(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"create_import_job", "get_import_job"}, OperationWriteFamilies: []OperationFamily{OperationFamilyImports}}
	defs := make(map[string]operationalDefinition)
	for _, def := range operationalCatalog(opts, true) {
		defs[def.definition.name] = def
	}
	create, ok := defs["create_import_job"]
	requirements.True(ok)
	read, ok := defs["get_import_job"]
	requirements.True(ok)
	assertions.True(*create.definition.annotations.OpenWorldHint)
	assertions.Contains(create.definition.inputSchema.Required, "limit")
	assertions.Equal(new(float64(1)), create.definition.inputSchema.Properties["limit"].Minimum)
	assertions.Equal("^[0-9]{4}-[0-9]{2}-[0-9]{2}$", create.definition.inputSchema.Properties["after"].Pattern)
	assertions.Equal("^[0-9]{4}-[0-9]{2}-[0-9]{2}$", create.definition.inputSchema.Properties["before"].Pattern)
	assertions.Contains(read.definition.inputSchema.Required, "job_id")
}
