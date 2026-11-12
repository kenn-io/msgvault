package mcp

import (
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// EmploymentRecord exposes the native revision token alongside its record.
type EmploymentRecord struct {
	ETag       string               `json:"etag"`
	Employment generated.Employment `json:"employment"`
}

func employmentOperationalDefinitions() []operationalDefinition {
	id := safeIDSchema("Employment ID")
	etag := stringSchema("Exact ETag from the latest employment read")
	body := closedObject(map[string]*jsonschema.Schema{
		"person_id": safeIDSchema("Durable person ID"), "organization_id": safeIDSchema("Organization ID"),
		"source": stringSchema("Native provenance", "user", "carddav_import", "vcard_import", "archive_observation", "extraction", "enrichment", "system"),
		"title":  stringSchema("Title"), "role": stringSchema("Role"), "department": stringSchema("Department"), "location": stringSchema("Location"),
		"address_id": safeIDSchema("Address ID"), "description": stringSchema("Description"),
		"start_date": stringSchema("Partial ISO date: YYYY, YYYY-MM, or YYYY-MM-DD"), "end_date": stringSchema("Partial ISO date"),
		"is_current": booleanSchema("Explicit current state"), "is_primary": booleanSchema("Explicit primary state"),
		"source_ref": stringSchema("Provenance reference"), "confidence": {Type: "number"},
	}, "person_id", "organization_id", "source")
	// Native nullable fields use omission and null equivalently for replacement.
	for key := range body.Properties {
		if slices.Contains(body.Required, key) {
			continue
		}
		body.Properties[key] = &jsonschema.Schema{AnyOf: []*jsonschema.Schema{body.Properties[key], {Type: "null"}}}
	}
	record := outputSchemaFor[EmploymentRecord]()
	list := outputSchemaFor[generated.EmploymentsResponse]()
	mutation := func(name, description string, properties map[string]*jsonschema.Schema, required ...string) operationalDefinition {
		return newOperationalDefinition(name, description+" Requires client confirmation.", OperationFamilyEmployment, closedObject(properties, required...), record, true)
	}
	listInput := func(key string) *jsonschema.Schema {
		return closedObject(map[string]*jsonschema.Schema{key: safeIDSchema("Exact owner ID"), "current_only": booleanSchema("Only current records"), "limit": boundedIntegerSchema("Maximum results", 0, 200), "offset": boundedIntegerSchema("Results to skip", 0, maxJSONSafeInteger)}, key)
	}
	return []operationalDefinition{
		newOperationalDefinition("get_employment", "Read one employment and its exact revision ETag.", OperationFamilyEmployment, closedObject(map[string]*jsonschema.Schema{"employment_id": id}, "employment_id"), record, false),
		newOperationalDefinition("list_person_employments", "List a person's employment history and current primary projection.", OperationFamilyEmployment, listInput("person_id"), list, false),
		newOperationalDefinition("list_organization_employments", "List an organization's employment history.", OperationFamilyEmployment, listInput("organization_id"), list, false),
		mutation("create_employment", "Create an employment record; native primary and duplicate guards apply.", map[string]*jsonschema.Schema{"employment": body}, "employment"),
		mutation("replace_employment", "Replace mutable fields using the exact ETag; omitted text, dates and other optional fields are cleared, while omitted current and primary flags follow native preservation rules.", map[string]*jsonschema.Schema{"employment_id": id, "etag": etag, "employment": body}, "employment_id", "etag", "employment"),
		mutation("end_employment", "End the employment on a partial date using the exact ETag.", map[string]*jsonschema.Schema{"employment_id": id, "etag": etag, "end_date": stringSchema("Partial ISO end date")}, "employment_id", "etag", "end_date"),
		mutation("set_primary_employment", "Set this current employment primary using the exact ETag; another primary may be demoted.", map[string]*jsonschema.Schema{"employment_id": id, "etag": etag}, "employment_id", "etag"),
		newOperationalDefinition("delete_employment", "Delete this employment using the exact ETag. Requires client confirmation.", OperationFamilyEmployment, closedObject(map[string]*jsonschema.Schema{"employment_id": id, "etag": etag}, "employment_id", "etag"), closedObject(nil), true),
	}
}
