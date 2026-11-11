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
	return []operationalDefinition{
		newOperationalDefinition("get_organization", "Read an organization profile and its exact ETag.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"organization_id": id}, "organization_id"), outputSchemaFor[OrganizationRecord](), false),
		newOperationalDefinition("get_organization_history", "Read an organization's profile history and exact ETag.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"organization_id": id}, "organization_id"), outputSchemaFor[OrganizationRecord](), false),
		newOperationalDefinition("list_organization_attributes", "Read an organization's typed attributes, optionally including superseded values.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"organization_id": id, "definition_slug": stringSchema("Optional exact definition slug"), "include_superseded": booleanSchema("Include historical values")}, "organization_id"), outputSchemaFor[generated.OrganizationAttributesResponse](), false),
	}
}
