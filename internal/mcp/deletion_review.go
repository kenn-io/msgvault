package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func deletionReviewOperationalDefinitions() []operationalDefinition {
	status := stringSchema("Optional persisted batch status")
	for _, value := range deletion.PersistedStatuses() {
		status.Enum = append(status.Enum, string(value))
	}
	return []operationalDefinition{
		newOperationalDefinition("list_deletions", "List staged deletion batches, newest first, optionally filtered by status.", OperationFamilyDeletions, closedObject(map[string]*jsonschema.Schema{"status": status}), outputSchemaFor[generated.ListDeletionsResponse](), false),
		newOperationalDefinition("get_deletion", "Read a staged deletion batch lifecycle receipt; the complete selected message ID set is not included.", OperationFamilyDeletions, closedObject(map[string]*jsonschema.Schema{"manifest_id": stringSchema("Exact deletion manifest ID")}, "manifest_id"), outputSchemaFor[generated.DeletionManifestDetail](), false),
	}
}
