package mcp

import (
	"encoding/json/v2"
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type OrganizationRecord struct {
	PersonRecordChunk `json:",inline"`
	ETag              string                         `json:"etag"`
	Profile           *generated.OrganizationProfile `json:"profile,omitzero"`
}

type OrganizationAttributesResult struct {
	PersonRecordChunk `json:",inline"`
	Values            []generated.OrganizationAttributeValue `json:"values,omitzero"`
}

func organizationOperationalDefinitions() []operationalDefinition {
	id := safeIDSchema("Durable organization ID")
	offset, length := chunkInputSchemas("organization profile")
	offset.Default, length.Default = nil, nil
	definitions := []operationalDefinition{
		newOperationalDefinition("list_organizations", "Find organizations by name, including organizations without employees.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"q": stringSchema("Name search"), "include_retired": booleanSchema("Include retired organizations"), "limit": boundedIntegerSchema("Maximum results", 1, 1000), "offset": boundedIntegerSchema("Results to skip", 0, 9007199254740991)}), outputSchemaFor[generated.OrganizationsResponse](), false),
		newOperationalDefinition("get_organization", "Read an organization profile and ETag; include superseded values when requested. Large profiles return chunks; pass the SHA-256 with subsequent offsets.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"organization_id": id, "include_superseded": booleanSchema("Include historical values"), "offset": offset, "length": length, "sha256": stringSchema("Whole-object SHA-256")}, "organization_id"), outputSchemaFor[OrganizationRecord](), false),
		newOperationalDefinition("list_organization_attributes", "Read an organization's typed attributes. Large histories return chunks; pass the SHA-256 with subsequent offsets.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"organization_id": id, "definition_slug": stringSchema("Optional exact definition slug"), "include_superseded": booleanSchema("Include historical values"), "offset": offset, "length": length, "sha256": stringSchema("Whole-object SHA-256")}, "organization_id"), organizationAttributesSchema(), false),
	}
	definitions[1].chunked = &chunkedOperation{
		inline: func(s *downloadSnapshot) (any, error) {
			var p generated.OrganizationProfile
			err := json.Unmarshal(s.data, &p)
			return OrganizationRecord{ETag: s.etag, Profile: &p}, err
		},
		chunk: func(s *downloadSnapshot, c byteChunk) any {
			return OrganizationRecord{ETag: s.etag, PersonRecordChunk: personRecordChunk(c)}
		},
	}
	definitions[2].chunked = &chunkedOperation{
		inline: func(s *downloadSnapshot) (any, error) {
			var attributes generated.OrganizationAttributesResponse
			err := json.Unmarshal(s.data, &attributes)
			return OrganizationAttributesResult{Values: attributes.Values}, err
		},
		chunk: func(_ *downloadSnapshot, c byteChunk) any {
			return OrganizationAttributesResult{PersonRecordChunk: personRecordChunk(c)}
		},
	}
	return definitions
}

type OrganizationMutation struct {
	ETag         string                 `json:"etag"`
	Organization generated.Organization `json:"organization"`
}
type OrganizationDeleted struct {
	Deleted bool `json:"deleted"`
}
type OrganizationAttributeReceipt struct {
	ValueID      int64  `json:"value_id"`
	Ordinal      int64  `json:"ordinal"`
	SupersededID *int64 `json:"superseded_id,omitzero"`
}
type OrganizationCreateArgs struct {
	Body generated.OrganizationCreateBody `json:"body"`
}
type OrganizationUpdateArgs struct {
	OrganizationID int64                      `json:"organization_id"`
	ETag           string                     `json:"etag"`
	Body           generated.OrganizationBody `json:"body"`
}
type OrganizationDeleteArgs struct {
	OrganizationID int64  `json:"organization_id"`
	ETag           string `json:"etag"`
}
type OrganizationProfileArgs struct {
	OrganizationID int64                             `json:"organization_id"`
	ETag           string                            `json:"etag"`
	Body           generated.OrganizationProfileBody `json:"body"`
}
type OrganizationSetAttributeArgs struct {
	OrganizationID int64                                  `json:"organization_id"`
	Body           generated.SetOrganizationAttributeBody `json:"body"`
}

func organizationInputSchema[T any](required ...string) *jsonschema.Schema {
	s := outputSchemaFor[T]()
	s.Required = required
	s.AdditionalProperties = rejectAllSchema()
	if _, ok := s.Properties["organization_id"]; ok {
		s.Properties["organization_id"] = safeIDSchema("Durable organization ID")
	}
	return s
}

func organizationWriteDefinitions() []operationalDefinition {
	destructive := func(definition operationalDefinition) operationalDefinition {
		definition.definition.annotations.DestructiveHint = new(true)
		return definition
	}
	return []operationalDefinition{
		newOperationalDefinition("create_organization", "Create an organization. Requires client confirmation.", OperationFamilyOrganizations, organizationInputSchema[OrganizationCreateArgs]("body"), outputSchemaFor[OrganizationMutation](), true),
		destructive(newOperationalDefinition("update_organization", "Replace mutable organization fields using the exact ETag. Omitted fields are cleared except retired. Requires client confirmation.", OperationFamilyOrganizations, organizationInputSchema[OrganizationUpdateArgs]("organization_id", "etag", "body"), outputSchemaFor[OrganizationMutation](), true)),
		destructive(newOperationalDefinition("delete_organization", "Delete an organization using its exact ETag. Native deletion refuses organizations with employment records. Requires client confirmation.", OperationFamilyOrganizations, organizationInputSchema[OrganizationDeleteArgs]("organization_id", "etag"), outputSchemaFor[OrganizationDeleted](), true)),
		destructive(newOperationalDefinition("replace_organization_profile", "Replace all profile collections using the exact ETag; omitted collections are cleared. Oversized saves return an ETag; use get_organization to read chunks. URIs are never fetched. Requires client confirmation.", OperationFamilyOrganizations, organizationInputSchema[OrganizationProfileArgs]("organization_id", "etag", "body"), outputSchemaFor[OrganizationRecord](), true)),
		destructive(newOperationalDefinition("set_organization_attribute", "Set an organization attribute with an explicit ordinal, expected value and dry-run controls. Returns a receipt; read values with list_organization_attributes. Requires client confirmation.", OperationFamilyOrganizations, organizationSetAttributeSchema(), outputSchemaFor[OrganizationAttributeReceipt](), true)),
	}
}

// jsontext.Value carries arbitrary JSON, not a base64 byte string.
func organizationAttributeValueSchema(s *jsonschema.Schema) {
	s.Properties["value"].Properties["json"] = &jsonschema.Schema{}
}

func organizationAttributesSchema() *jsonschema.Schema {
	s := outputSchemaFor[OrganizationAttributesResult]()
	organizationAttributeValueSchema(s.Properties["values"].Items)
	return s
}
func organizationSetAttributeSchema() *jsonschema.Schema {
	s := organizationInputSchema[OrganizationSetAttributeArgs]("organization_id", "body")
	organizationAttributeValueSchema(s.Properties["body"])
	s.Properties["body"].Required = append(s.Properties["body"].Required, "expected_value_id", "ordinal")
	return s
}
