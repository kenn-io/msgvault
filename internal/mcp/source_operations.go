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
		newOperationalDefinition("get_person_edit_context", "Read a durable person's identity, saved display name, revision and exact ETag for editing.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{"person_id": safeIDSchema("Durable person ID")}, "person_id"), outputSchemaFor[PersonEditContext](), false, false),
		newOperationalDefinition("set_person_display_name", "Set a durable person's display name using the exact ETag. An explicit empty string clears the saved name. Requires approval.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{
			"person_id": safeIDSchema("Durable person ID"), "etag": stringSchema("Exact ETag from get_person_edit_context"), "display_name": stringSchema("Saved name; empty clears it"),
		}, "person_id", "etag", "display_name"), outputSchemaFor[PersonEditContext](), true, false),
		newOperationalDefinition("get_source_scheduler_status", "Read source scheduler status, including queued and pending work. This does not start synchronization.", OperationFamilySources, closedObject(nil), outputSchemaFor[generated.SchedulerStatusResponse](), false, false),
		newOperationalDefinition("sync_source", "Request synchronization using the configured source scheduler. Generic sources run their configured shared source job; acceptance is not completion. Requires explicit approval.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{
			"account":     stringSchema("Exact configured source identifier from list_source_status"),
			"source_type": stringSchema("Exact source type; required for generic source jobs"),
		}, "account", "source_type"), outputSchemaFor[generated.StatusMessageResponse](), true, false),
	}
}
