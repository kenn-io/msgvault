package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type RelationshipTypeRecord struct {
	ETag             string                     `json:"etag"`
	RelationshipType generated.RelationshipType `json:"relationship_type"`
}
type PersonRelationshipRecord struct {
	ETag         string                       `json:"etag"`
	Relationship generated.PersonRelationship `json:"relationship"`
}

type RelationshipDeletion struct {
	Deleted bool `json:"deleted"`
}

func relationshipRecordDefinitions() []operationalDefinition {
	id := safeIDSchema("Positive durable record ID")
	person := safeIDSchema("Positive durable person ID")
	ended := booleanSchema("Include ended records")
	typeFields := map[string]*jsonschema.Schema{"forward_label": stringSchema("Forward label"), "reverse_label": stringSchema("Reverse label"), "vcard_related_type": stringSchema("vCard type"), "color": stringSchema("Color"), "icon": stringSchema("Icon"), "description": stringSchema("Description")}
	typeCreate := map[string]*jsonschema.Schema{"slug": stringSchema("Unique slug"), "forward_label": stringSchema("Forward label"), "reverse_label": stringSchema("Reverse label"), "is_symmetric": booleanSchema("Symmetric relationship"), "vcard_related_type": stringSchema("vCard type"), "color": stringSchema("Color"), "icon": stringSchema("Icon"), "description": stringSchema("Description")}
	typeFields["id"] = id
	typeFields["etag"] = stringSchema("Exact current ETag")
	recordCreate := map[string]*jsonschema.Schema{"source_person_id": person, "target_person_id": person, "relationship_type_slug": stringSchema("Exact relationship type slug"), "start_date": stringSchema("Partial date"), "end_date": stringSchema("Partial date"), "notes": stringSchema("Notes")}
	notes := &jsonschema.Schema{AnyOf: []*jsonschema.Schema{stringSchema("Notes"), {Type: "null"}}}
	recordPatch := map[string]*jsonschema.Schema{"id": id, "etag": stringSchema("Exact current ETag"), "end_date": stringSchema("Nonempty partial date"), "notes": notes}
	return []operationalDefinition{
		newOperationalDefinition("list_relationship_types", "List declared relationship types.", OperationFamilyRecords, closedObject(nil), outputSchemaFor[generated.RelationshipTypesResponse](), false),
		newOperationalDefinition("get_relationship_type", "Read a relationship type and its ETag.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"id": id}, "id"), outputSchemaFor[RelationshipTypeRecord](), false),
		newOperationalDefinition("get_person_relationship_record", "Read a declared person relationship and its ETag.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"id": id}, "id"), outputSchemaFor[PersonRelationshipRecord](), false),
		newOperationalDefinition("list_person_relationships", "List a person's declared relationships.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"person_id": person, "include_ended": ended}, "person_id"), outputSchemaFor[generated.PersonRelationshipsResponse](), false),
		newOperationalDefinition("list_person_relationship_reviews", "List imported relationship values awaiting or receiving review.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"person_id": person, "status": stringSchema("pending, accepted or rejected")}), outputSchemaFor[generated.RelationshipReviewsResponse](), false),
		newOperationalDefinition("get_person_network", "Read the bounded curated network, excluding archive-derived associations. Depth is 1 to 3.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"person_id": person, "depth": {Type: "integer"}, "include_ended": ended}, "person_id"), outputSchemaFor[generated.PersonNetwork](), false),
		newOperationalDefinition("create_relationship_type", "Create a relationship type. Requires approval.", OperationFamilyRelationshipRecords, closedObject(typeCreate, "slug", "forward_label", "reverse_label"), outputSchemaFor[RelationshipTypeRecord](), true),
		newOperationalDefinition("update_relationship_type", "Update fields on a relationship type using its exact ETag. Empty strings are explicit. Requires approval.", OperationFamilyRelationshipRecords, closedObject(typeFields, "id", "etag"), outputSchemaFor[RelationshipTypeRecord](), true),
		newOperationalDefinition("delete_relationship_type", "Delete an unused user relationship type using its exact ETag. Requires approval.", OperationFamilyRelationshipRecords, closedObject(map[string]*jsonschema.Schema{"id": id, "etag": stringSchema("Exact current ETag")}, "id", "etag"), outputSchemaFor[RelationshipDeletion](), true),
		newOperationalDefinition("create_person_relationship", "Declare a directed relationship between two people. Requires approval.", OperationFamilyRelationshipRecords, closedObject(recordCreate, "source_person_id", "target_person_id", "relationship_type_slug"), outputSchemaFor[PersonRelationshipRecord](), true),
		newOperationalDefinition("update_person_relationship", "End a relationship or update notes using its exact ETag. Null notes clear them. Requires approval.", OperationFamilyRelationshipRecords, closedObject(recordPatch, "id", "etag"), outputSchemaFor[PersonRelationshipRecord](), true),
		newOperationalDefinition("delete_person_relationship", "Delete a declared relationship using its exact ETag. Requires approval.", OperationFamilyRelationshipRecords, closedObject(map[string]*jsonschema.Schema{"id": id, "etag": stringSchema("Exact current ETag")}, "id", "etag"), outputSchemaFor[RelationshipDeletion](), true),
	}
}
