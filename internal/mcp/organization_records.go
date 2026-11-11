package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type OrganizationRecord struct {
	ETag    string                        `json:"etag"`
	Profile generated.OrganizationProfile `json:"profile"`
}

func organizationOperationalDefinitions() []operationalDefinition {
	id := safeIDSchema("Durable organization ID")
	offset, length := chunkInputSchemas("organization media")
	return []operationalDefinition{
		newOperationalDefinition("get_organization_media", "Read stored inline media as bounded base64 chunks. Start at offset zero; pass the returned SHA-256 for subsequent offsets. URI-only media is unavailable and is never fetched.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"organization_id": safeIDSchema("Organization ID"), "media_id": safeIDSchema("Profile media ID"), "offset": offset, "length": length, "sha256": stringSchema("SHA-256 from the first chunk")}, "organization_id", "media_id"), outputSchemaFor[PersonMediaContent](), false),
		newOperationalDefinition("get_organization", "Read an organization profile and its exact ETag.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"organization_id": id}, "organization_id"), outputSchemaFor[OrganizationRecord](), false),
		newOperationalDefinition("get_organization_history", "Read an organization's profile history and exact ETag.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"organization_id": id}, "organization_id"), outputSchemaFor[OrganizationRecord](), false),
		newOperationalDefinition("list_organization_attributes", "Read an organization's typed attributes, optionally including superseded values.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"organization_id": id, "definition_slug": stringSchema("Optional exact definition slug"), "include_superseded": booleanSchema("Include historical values")}, "organization_id"), organizationAttributesSchema(), false),
	}
}

type OrganizationMutation struct {
	ETag         string                 `json:"etag"`
	Organization generated.Organization `json:"organization"`
}
type OrganizationDeleted struct {
	Deleted bool `json:"deleted"`
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
type OrganizationMergeArgs struct {
	OrganizationID int64                           `json:"organization_id"`
	ETag           string                          `json:"etag"`
	Body           generated.MergeOrganizationBody `json:"body"`
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
type OrganizationClearAttributeArgs struct {
	OrganizationID  int64  `json:"organization_id"`
	Slug            string `json:"slug"`
	Ordinal         *int64 `json:"ordinal,omitempty"`
	ExpectedValueID *int64 `json:"expected_value_id,omitempty"`
	DryRun          *bool  `json:"dry_run,omitempty"`
}

func organizationInputSchema[T any](required ...string) *jsonschema.Schema {
	s := outputSchemaFor[T]()
	s.Required = required
	s.AdditionalProperties = rejectAllSchema()
	return s
}

func organizationWriteDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("create_organization", "Create an organization. Requires client confirmation.", OperationFamilyOrganizations, organizationInputSchema[OrganizationCreateArgs]("body"), outputSchemaFor[OrganizationMutation](), true),
		newOperationalDefinition("update_organization", "Replace mutable organization fields using the exact ETag. Omitted fields are cleared except retired. Requires client confirmation.", OperationFamilyOrganizations, organizationInputSchema[OrganizationUpdateArgs]("organization_id", "etag", "body"), outputSchemaFor[OrganizationMutation](), true),
		newOperationalDefinition("delete_organization", "Delete an organization using its exact ETag. Native deletion refuses organizations with employment records. Requires client confirmation.", OperationFamilyOrganizations, organizationInputSchema[OrganizationDeleteArgs]("organization_id", "etag"), outputSchemaFor[OrganizationDeleted](), true),
		newOperationalDefinition("merge_organization", "Merge a losing organization into the survivor, guarded by both revisions. Requires client confirmation.", OperationFamilyOrganizations, organizationInputSchema[OrganizationMergeArgs]("organization_id", "etag", "body"), outputSchemaFor[OrganizationMutation](), true),
		newOperationalDefinition("replace_organization_profile", "Replace all profile collections using the exact ETag; omitted collections are cleared. Inline data is bounded and URIs are never fetched. Requires client confirmation.", OperationFamilyOrganizations, organizationInputSchema[OrganizationProfileArgs]("organization_id", "etag", "body"), outputSchemaFor[OrganizationRecord](), true),
		newOperationalDefinition("set_organization_attribute", "Set an organization attribute with native ordinal, expected value and dry-run controls. Requires client confirmation.", OperationFamilyOrganizations, organizationSetAttributeSchema(), organizationAttributeWriteSchema(), true),
		newOperationalDefinition("clear_organization_attribute", "Clear an organization attribute with native ordinal, expected value and dry-run controls. Requires client confirmation.", OperationFamilyOrganizations, organizationInputSchema[OrganizationClearAttributeArgs]("organization_id", "slug"), organizationAttributeWriteSchema(), true),
	}
}

// jsontext.Value carries arbitrary JSON, not a base64 byte string.
func organizationAttributeValueSchema(s *jsonschema.Schema) {
	s.Properties["value"].Properties["json"] = &jsonschema.Schema{}
}

func organizationAttributesSchema() *jsonschema.Schema {
	s := outputSchemaFor[generated.OrganizationAttributesResponse]()
	organizationAttributeValueSchema(s.Properties["values"].Items)
	return s
}
func organizationAttributeWriteSchema() *jsonschema.Schema {
	s := outputSchemaFor[generated.OrganizationAttributeWrite]()
	organizationAttributeValueSchema(s.Properties["value"])
	organizationAttributeValueSchema(s.Properties["superseded"])
	return s
}
func organizationSetAttributeSchema() *jsonschema.Schema {
	s := organizationInputSchema[OrganizationSetAttributeArgs]("organization_id", "body")
	organizationAttributeValueSchema(s.Properties["body"])
	return s
}
