package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

// Catalog capability fixture only; behavioral coverage uses the native daemon
// and Store in cmd/mcp_collection_scopes_test.go.
type collectionCatalogEngine struct{ *querytest.MockEngine }

func (collectionCatalogEngine) ListCollectionScopes(context.Context) ([]query.CollectionScope, error) {
	return []query.CollectionScope{}, nil
}

func TestCollectionCatalogUsesImmutableReadSchemaAndOptionalOwnerCapability(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	opts := ServeOptions{Engine: collectionCatalogEngine{&querytest.MockEngine{}}}
	tools := toolsByName(t, rawListTools(t, opts, false))
	tool, exists := tools["list_message_collections"]
	requirements.True(exists)
	annotations, ok := tool["annotations"].(map[string]any)
	requirements.True(ok)
	assertions.Equal(true, annotations["readOnlyHint"])
	for _, name := range []string{ToolSearchMetadata, ToolSearchMessages, ToolListMessages, ToolAggregate, ToolGetStats, ToolSearchMessageBodies} {
		assertions.Equal("string", toolInputProperty(t, tools[name], "collection")["type"])
	}
	writes := toolsByName(t, rawListTools(t, opts, true))
	assertions.Equal("string", toolInputProperty(t, writes[ToolStageDeletion], "collection")["type"])
	first := operationCatalog(opts, nil)
	second := operationCatalog(opts, nil)
	for i := range first {
		assertions.Same(first[i].inputSchema, second[i].inputSchema, "immutable schema %s", first[i].name)
	}
	unsupported := toolsByName(t, rawListTools(t, ServeOptions{Engine: &querytest.MockEngine{}}, false))
	_, exists = unsupported["list_message_collections"]
	assertions.False(exists)
	opts.DelegatedOnly = true
	delegated := toolsByName(t, rawListTools(t, opts, false))
	_, exists = delegated["list_message_collections"]
	assertions.False(exists)
}
