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

func relationshipRecordDefinitions() []operationalDefinition {
	id := safeIDSchema("Positive durable record ID")
	person := safeIDSchema("Positive durable person ID")
	ended := booleanSchema("Include ended records")
	return []operationalDefinition{
		newOperationalDefinition("list_relationship_types", "List declared relationship types.", OperationFamilyRecords, closedObject(nil), outputSchemaFor[generated.RelationshipTypesResponse](), false),
		newOperationalDefinition("get_relationship_type", "Read a relationship type and its ETag.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"id": id}, "id"), outputSchemaFor[RelationshipTypeRecord](), false),
		newOperationalDefinition("get_person_relationship_record", "Read a declared person relationship and its ETag.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"id": id}, "id"), outputSchemaFor[PersonRelationshipRecord](), false),
		newOperationalDefinition("list_person_relationships", "List a person's declared relationships.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"person_id": person, "include_ended": ended}, "person_id"), outputSchemaFor[generated.PersonRelationshipsResponse](), false),
		newOperationalDefinition("list_person_relationship_reviews", "List imported relationship values awaiting or receiving review.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"person_id": person, "status": stringSchema("pending, accepted or rejected")}), outputSchemaFor[generated.RelationshipReviewsResponse](), false),
		newOperationalDefinition("get_person_network", "Read the bounded curated network, excluding archive-derived associations. Depth is 1 to 3.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"person_id": person, "depth": {Type: "integer"}, "include_ended": ended}, "person_id"), outputSchemaFor[generated.PersonNetwork](), false),
	}
}
