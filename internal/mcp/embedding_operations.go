package mcp

import "go.kenn.io/msgvault/internal/vector"

func embeddingOperationalDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("list_embedding_generations", "Read text embedding generation metadata, resolved coverage scope, lifecycle, coverage and accelerator state from the owning daemon. Retired coverage is explicitly unavailable. Does not call an embedding provider; takes no host path or command arguments.", OperationFamilySources, closedObject(nil), outputSchemaFor[vector.GenerationStatusReport](), false, false),
	}
}
