package mcp

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDeletionNativeCatalogKeepsExistingWriteGateAndImmutableRoots(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"preview_deletion_selection", ToolStageDeletion}}
	readonly := toolsByName(t, rawListTools(t, opts, false))
	assertions.Contains(readonly, "preview_deletion_selection")
	assertions.NotContains(readonly, ToolStageDeletion)
	writes := toolsByName(t, rawListTools(t, opts, true))
	requirements.Contains(writes, ToolStageDeletion)
	assertions.Equal("array", toolInputProperty(t, writes[ToolStageDeletion], "message_ids")["type"])
	assertions.Equal("object", toolInputProperty(t, writes[ToolStageDeletion], "selection")["type"])
	assertions.Equal("boolean", toolInputProperty(t, writes[ToolStageDeletion], "dry_run")["type"])
	first := operationalCatalog(opts, true)
	second := operationalCatalog(opts, true)
	for i := range first {
		assertions.Same(first[i].definition.inputSchema, second[i].definition.inputSchema)
	}
	opts.OperationCapabilities = nil
	absent := toolsByName(t, rawListTools(t, opts, true))
	assertions.NotContains(absent, ToolStageDeletion)
	assertions.NotContains(absent, "preview_deletion_selection")
	opts.OperationCapabilities = []string{"preview_deletion_selection", ToolStageDeletion}
	opts.DelegatedOnly = true
	delegated := toolsByName(t, rawListTools(t, opts, true))
	assertions.NotContains(delegated, ToolStageDeletion)
	assertions.NotContains(delegated, "preview_deletion_selection")
}
