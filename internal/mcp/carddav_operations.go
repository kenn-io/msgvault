package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// CardDAVConnection omits the configured OAuth application identifier.
type CardDAVConnection struct {
	AccountID  *int64                          `json:"account_id,omitempty"`
	Connection string                          `json:"connection"`
	Orphaned   bool                            `json:"orphaned"`
	Provider   *string                         `json:"provider,omitempty"`
	Status     generated.CardDAVStatusResponse `json:"status"`
}

// CardDAVConnections contains the public connection projections.
type CardDAVConnections struct {
	Connections []CardDAVConnection `json:"connections"`
}

func cardDAVOperationalDefinitions() []operationalDefinition {
	connection := stringSchema("Exact saved connection name; omit for all")
	id := safeIDSchema("Durable ID from the corresponding CardDAV list")
	definitions := []operationalDefinition{
		newOperationalDefinition("list_carddav_connections", "List saved CardDAV connections and their current status.", OperationFamilyCardDAV, closedObject(nil), outputSchemaFor[CardDAVConnections](), false),
		newOperationalDefinition("list_carddav_books", "List address books and their durable IDs and roles.", OperationFamilyCardDAV, closedObject(map[string]*jsonschema.Schema{"connection": connection}), outputSchemaFor[generated.CardDAVBooksResponse](), false),
		newOperationalDefinition("list_carddav_runs", "List sync runs newest first; use next_before_id for the next page.", OperationFamilyCardDAV, closedObject(map[string]*jsonschema.Schema{"connection": connection, "before_id": id, "limit": {Type: "integer", Minimum: new(float64(1)), Maximum: new(float64(100))}}), outputSchemaFor[generated.CardDAVRunsResponse](), false),
		newOperationalDefinition("list_carddav_conflicts", "List unresolved conflicts and durable IDs.", OperationFamilyCardDAV, closedObject(nil), outputSchemaFor[generated.CardDAVConflictsResponse](), false),
		newOperationalDefinition("get_carddav_conflict", "Inspect bounded base, local and remote contact summaries for a conflict.", OperationFamilyCardDAV, closedObject(map[string]*jsonschema.Schema{"conflict_id": id}, "conflict_id"), outputSchemaFor[generated.CardDAVConflictDetailResponse](), false),
		newOperationalDefinition("update_carddav_book_roles", "Replace all roles of one address book. Setting a write target transfers it from its previous book; disabling roles can remove local mappings. Requires approval.", OperationFamilyCardDAV, closedObject(map[string]*jsonschema.Schema{"book_id": id, "write_target": booleanSchema("Use as publication target"), "subscribed": booleanSchema("Import contacts"), "lookup_source": booleanSchema("Use for lookup")}, "book_id", "write_target", "subscribed", "lookup_source"), outputSchemaFor[generated.CardDAVBookResponse](), true),
		newOperationalDefinition("resolve_carddav_conflict", "Resolve a conflict by keeping its local or remote version. Requires approval and may create, update or remove a remote contact during this call.", OperationFamilyCardDAV, closedObject(map[string]*jsonschema.Schema{"conflict_id": id, "choice": stringSchema("keep_local or keep_remote")}, "conflict_id", "choice"), outputSchemaFor[generated.CardDAVConflictResolutionResponse](), true),
		newOperationalDefinition("unpublish_carddav_person", "Unpublish a person and remove the remote contact while retaining the local person. Requires approval.", OperationFamilyCardDAV, closedObject(map[string]*jsonschema.Schema{"person_id": id}, "person_id"), outputSchemaFor[generated.CardDAVPublicationResponse](), true),
	}
	for i := range definitions {
		if definitions[i].definition.name == "unpublish_carddav_person" || definitions[i].definition.name == "resolve_carddav_conflict" {
			definitions[i].definition.annotations.DestructiveHint = new(true)
		}
	}
	return definitions
}
