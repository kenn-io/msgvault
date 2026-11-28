package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func deletionReviewOperationalDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("list_deletions", "List staged deletion batches, newest first, optionally filtered by status.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"status": stringSchema("Optional batch status: pending, in_progress, completed, failed, or cancelled")}), outputSchemaFor[generated.ListDeletionsResponse](), false),
		newOperationalDefinition("get_deletion", "Read a staged deletion batch lifecycle receipt, which may include failed or checkpoint IDs and is not a selection preview.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"batch_id": stringSchema("Batch ID from stage_deletion or id from list_deletions")}, "batch_id"), outputSchemaFor[generated.DeletionManifestDetail](), false),
	}
}
