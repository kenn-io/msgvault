package mcp

import (
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/store"
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
	sources := make([]string, 0, len(store.AllProvenances))
	for _, source := range store.AllProvenances {
		sources = append(sources, string(source))
	}
	body := outputSchemaFor[generated.EmploymentBody]()
	body.Schema = ""
	body.Properties["person_id"] = safeIDSchema("Durable person ID")
	body.Properties["organization_id"] = safeIDSchema("Organization ID")
	body.Properties["address_id"] = safeIDSchema("Address ID")
	body.Properties["source"] = stringSchema("Native provenance", sources...)
	body.Properties["confidence"] = scoreSchema("Derived confidence from 0 to 1")
	body.Properties["start_date"] = stringSchema("Partial ISO date: YYYY, YYYY-MM, or YYYY-MM-DD")
	body.Properties["end_date"] = stringSchema("Partial ISO date")
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
		mutation("replace_employment", "Replace mutable fields using the exact ETag; omitted text, dates and other optional fields are cleared, while omitted current and primary flags follow native preservation rules. Reads return dates as year/month/day objects; send partial ISO date strings.", map[string]*jsonschema.Schema{"employment_id": id, "etag": etag, "employment": body}, "employment_id", "etag", "employment"),
		mutation("end_employment", "End the employment on a partial date using the exact ETag.", map[string]*jsonschema.Schema{"employment_id": id, "etag": etag, "end_date": stringSchema("Partial ISO end date")}, "employment_id", "etag", "end_date"),
		mutation("set_primary_employment", "Set this current employment primary using the exact ETag; another primary may be demoted.", map[string]*jsonschema.Schema{"employment_id": id, "etag": etag}, "employment_id", "etag"),
		newOperationalDefinition("delete_employment", "Delete this employment using the exact ETag. Requires client confirmation.", OperationFamilyEmployment, closedObject(map[string]*jsonschema.Schema{"employment_id": id, "etag": etag}, "employment_id", "etag"), closedObject(nil), true),
	}
}
