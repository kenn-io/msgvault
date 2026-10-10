package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type SlackSyncPolicy struct {
	ETag           string              `json:"etag"`
	PendingRestart bool                `json:"pending_restart"`
	Settings       []generated.Setting `json:"settings"`
}

type PersonEditContext struct {
	ETag   string           `json:"etag"`
	Person generated.Person `json:"person"`
}

type PersonProfileEditContext struct {
	ETag    string                            `json:"etag"`
	Profile generated.StructuredPersonProfile `json:"profile"`
}

type PersonAttributeEditContext struct {
	ETag       string                             `json:"etag"`
	Attributes generated.PersonAttributesResponse `json:"attributes"`
}

type personAttributeSchemaArguments struct {
	PersonID        int64                     `json:"person_id"`
	ETag            string                    `json:"etag"`
	AttributeSlug   string                    `json:"attribute_slug"`
	ExpectedValueID int64                     `json:"expected_value_id"`
	Ordinal         *int64                    `json:"ordinal,omitzero"`
	Value           *generated.AttributeValue `json:"value,omitzero"`
}

func personAttributeInputSchema(isClear bool) *jsonschema.Schema {
	schema := personAttributeOutputSchema[personAttributeSchemaArguments]()
	schema.Properties[toolArgPersonID] = safeIDSchema("Durable person ID")
	schema.Properties["etag"] = stringSchema("Exact ETag from get_person_attributes; value-slot CAS is separate")
	schema.Properties["expected_value_id"] = safeIDSchema("Exact current value ID; zero requires an empty slot for a set")
	schema.Properties["ordinal"] = safeIDSchema("Explicit ordinal for a multi-valued slot; zero is allowed")
	schema.Properties["ordinal"].Minimum = new(float64(0))
	if isClear {
		delete(schema.Properties, "value")
	} else {
		schema.Properties["expected_value_id"].Minimum = new(float64(0))
		schema.Required = append(schema.Required, "value")
		primitiveAttributeValueSchema(schema)
	}
	return schema
}

// AttributeValue.JSON is raw JSON on the wire, rather than the reflected
// byte slice. Bound nested numbers to the MCP transport's exact integer range.
func personAttributeOutputSchema[T any]() *jsonschema.Schema {
	schema := outputSchemaFor[T]()
	correctAttributeValueSchemas(schema)
	if schema.Defs == nil {
		schema.Defs = make(map[string]*jsonschema.Schema)
	}
	jsonRef := func() *jsonschema.Schema { return &jsonschema.Schema{Ref: "#/$defs/MCPAttributeJSON"} }
	object := closedObject(nil)
	object.AdditionalProperties = jsonRef()
	schema.Defs["MCPAttributeJSON"] = &jsonschema.Schema{AnyOf: []*jsonschema.Schema{
		{Type: "null"}, {Type: "string"}, {Type: "boolean"},
		{Type: "number", Minimum: new(-maxJSONSafeInteger), Maximum: new(maxJSONSafeInteger)},
		{Type: "array", Items: jsonRef()},
		object,
	}}
	return schema
}

func correctAttributeValueSchemas(schema *jsonschema.Schema) {
	if schema == nil {
		return
	}
	if schema.Properties["record_id"] != nil {
		schema.Properties["integer"] = boundedIntegerSchema("Exact signed MCP integer", -maxJSONSafeInteger, maxJSONSafeInteger)
		schema.Properties["json"] = &jsonschema.Schema{Ref: "#/$defs/MCPAttributeJSON"}
	}
	for _, children := range []map[string]*jsonschema.Schema{schema.Properties, schema.Defs, schema.Definitions} {
		for _, child := range children {
			correctAttributeValueSchemas(child)
		}
	}
	for _, children := range [][]*jsonschema.Schema{schema.AllOf, schema.AnyOf, schema.OneOf} {
		for _, child := range children {
			correctAttributeValueSchemas(child)
		}
	}
	correctAttributeValueSchemas(schema.Items)
}

func primitiveAttributeValueSchema(schema *jsonschema.Schema) {
	if schema == nil {
		return
	}
	if schema.Properties["record_id"] != nil {
		delete(schema.Properties, "record_id")
		delete(schema.Properties, "record_type")
		schema.Properties["type"].Enum = []any{"text", "integer", "real", "boolean", "date", "timestamp", "json"}
	}
	for _, children := range []map[string]*jsonschema.Schema{schema.Properties, schema.Defs, schema.Definitions} {
		for _, child := range children {
			primitiveAttributeValueSchema(child)
		}
	}
}

type personProfilePatchSchemaArguments struct {
	PersonID int64                               `json:"person_id"`
	ETag     string                              `json:"etag"`
	Patch    generated.PersonProfilePatchRequest `json:"patch"`
}

func personProfilePatchInputSchema() *jsonschema.Schema {
	schema := outputSchemaFor[personProfilePatchSchemaArguments]()
	schema.Properties[toolArgPersonID] = safeIDSchema("Durable person ID")
	schema.Properties["etag"] = stringSchema("Exact ETag from get_person_structured_profile")
	boundProfileSupersedeSchemas(schema)
	return schema
}

func boundProfileSupersedeSchemas(schema *jsonschema.Schema) {
	if schema == nil {
		return
	}
	if supersede := schema.Properties["supersede"]; supersede != nil {
		supersede.Items = safeIDSchema("Exact native profile value ID")
	}
	for _, children := range []map[string]*jsonschema.Schema{schema.Properties, schema.Defs, schema.Definitions} {
		for _, child := range children {
			boundProfileSupersedeSchemas(child)
		}
	}
	for _, children := range [][]*jsonschema.Schema{schema.AllOf, schema.AnyOf, schema.OneOf} {
		for _, child := range children {
			boundProfileSupersedeSchemas(child)
		}
	}
	boundProfileSupersedeSchemas(schema.Items)
}

// The generated Settings value uses a private JSON union whose fields are not
// visible to reflection. Declare its real wire variants for MCP validation.
func slackPolicyOutputSchema() *jsonschema.Schema {
	schema := outputSchemaFor[SlackSyncPolicy]()
	schema.Properties["settings"].Items.Properties["value"] = &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		closedObject(map[string]*jsonschema.Schema{"boolean": booleanSchema("Selected boolean value")}, "boolean"),
		closedObject(map[string]*jsonschema.Schema{"strings": {Type: mcpSchemaArray, Items: stringSchema("Selected channel")}}, "strings"),
	}}
	return schema
}

func sourceOperationalDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("list_source_status", "Read source synchronization capabilities, latest runs, and queued or pending scheduler work.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{"source_type": stringSchema("Optional exact source type")}), outputSchemaFor[generated.SourceStatusResponse](), false, false),
		newOperationalDefinition("get_source_identities", "Read confirmed sender identities and confirmation signals for one source.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{"source_id": safeIDSchema("Exact archived source ID")}, "source_id"), outputSchemaFor[generated.SourceIdentitiesResponse](), false, false),
		newOperationalDefinition("get_slack_sync_policy", "Read Slack channel and direct-message selection, exact settings ETag and restart state. No credentials or unrelated settings are returned.", OperationFamilySources, closedObject(nil), slackPolicyOutputSchema(), false, false),
		newOperationalDefinition("update_slack_sync_policy", "Change Slack channel and direct-message selection using the exact ETag. Omitted fields are preserved; false and empty arrays are explicit changes. Requires approval and may require daemon restart.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{
			"etag":             stringSchema("Exact ETag from get_slack_sync_policy"),
			"dms":              booleanSchema("Include one-to-one direct messages"),
			"group_dms":        booleanSchema("Include group direct messages"),
			"channels":         {Type: mcpSchemaArray, Items: stringSchema("Exact channel selector")},
			"exclude_channels": {Type: mcpSchemaArray, Items: stringSchema("Exact excluded channel selector")},
		}, "etag"), slackPolicyOutputSchema(), true, false),
		newOperationalDefinition("get_participant_identity", "Read an observed participant's identifiers, service and scope context, cluster members and link-origin evidence.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{"participant_id": safeIDSchema("Observed participant ID")}, "participant_id"), outputSchemaFor[generated.PersonSummary](), false, false),
		newOperationalDefinition("get_cache_build_status", "Observe one accepted analytics cache build job. Unknown, restarted or evicted jobs return a refusal; this call does not start or repair a build.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{"job_id": stringSchema("Opaque job ID from a fresh query response")}, "job_id"), outputSchemaFor[generated.CacheBuildStatus](), false, false),
		newOperationalDefinition("get_person_edit_context", "Read a durable person's identity, saved display name, revision and exact ETag for editing.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{toolArgPersonID: safeIDSchema("Durable person ID")}, toolArgPersonID), outputSchemaFor[PersonEditContext](), false, true),
		newOperationalDefinition("set_person_display_name", "Set a durable person's display name using the exact ETag. An explicit empty string clears the saved name. Requires approval.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{
			toolArgPersonID: safeIDSchema("Durable person ID"), "etag": stringSchema("Exact ETag from get_person_edit_context"), "display_name": stringSchema("Saved name; empty clears it"),
		}, toolArgPersonID, "etag", "display_name"), outputSchemaFor[PersonEditContext](), true, true),
		newOperationalDefinition("get_person_structured_profile", "Read one durable person's structured names, contact points, addresses, dates, categories and media metadata with the exact ETag for editing.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{toolArgPersonID: safeIDSchema("Durable person ID")}, toolArgPersonID), outputSchemaFor[PersonProfileEditContext](), false, true),
		newOperationalDefinition("patch_person_profile", "Apply explicit structured profile additions and supersessions using the exact ETag. The saved display-name override is preserved. Requires approval.", OperationFamilyRecords, personProfilePatchInputSchema(), outputSchemaFor[PersonProfileEditContext](), true, true),
		newOperationalDefinition("get_person_attributes", "Read one durable person's typed attributes and native definitions with the exact person ETag and current value IDs for editing.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{toolArgPersonID: safeIDSchema("Durable person ID")}, toolArgPersonID), personAttributeOutputSchema[PersonAttributeEditContext](), false, true),
		newOperationalDefinition("set_person_attribute", "Set a primitive custom field with user provenance through native validation and value-slot compare-and-swap. Zero requires an empty slot; multi-valued creation requires an explicit ordinal. Requires approval.", OperationFamilyRecords, personAttributeInputSchema(false), personAttributeOutputSchema[generated.PersonAttributeWrite](), true, true),
		newOperationalDefinition("clear_person_attribute", "Supersede an exact current attribute value through the native history-preserving clear operation. Requires the person ETag, current value ID and approval.", OperationFamilyRecords, personAttributeInputSchema(true), personAttributeOutputSchema[generated.PersonAttributeWrite](), true, true),
		newOperationalDefinition("get_source_scheduler_status", "Read source scheduler status, including queued and pending work. This does not start synchronization.", OperationFamilySources, closedObject(nil), outputSchemaFor[generated.SchedulerStatusResponse](), false, false),
		newOperationalDefinition("sync_source", "Request synchronization using the configured source scheduler. Generic sources run their configured shared source job; acceptance is not completion. Requires explicit approval.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{
			"account":     stringSchema("Exact configured source identifier from list_source_status"),
			"source_type": stringSchema("Exact source type; required for generic source jobs"),
		}, "account", "source_type"), outputSchemaFor[generated.StatusMessageResponse](), true, false),
	}
}
