package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// OperationalSettingKeys is the literal inventory shared by schemas and the typed adapter. It
// excludes credentials, paths, destinations, trust and provider setup.
func OperationalSettingKeys() []string {
	return []string{
		"web.default_search_mode", "web.theme", "web.density",
		"server.daemon_idle_timeout", "server.daemon_auto_start", "server.daemon_auto_restart",
		"analytics.engine", "analytics.auto_build_cache", "analytics.min_rebuild_interval", "analytics.builder_memory_limit", "analytics.builder_threads", "analytics.builder_temp_limit",
		"sync.rate_limit_qps", "activity.timezone", "activity.max_direct_counterparts", "activity.batch_size", "activity.schedule", "log.enabled", "log.level",
		"beeper.enabled", "beeper.schedule", "beeper.accounts", "beeper.exclude_accounts", "beeper.rate_limit_qps",
		"slack.enabled", "slack.schedule", "slack.channels", "slack.exclude_channels", "slack.dms", "slack.group_dms",
		"beeper.media", "beeper.media_scope", "beeper.media_max_participants", "beeper.max_media_mb",
		"slack.media", "slack.media_scope", "slack.media_max_participants", "slack.max_media_mb",
		"discord.media", "discord.media_scope", "discord.media_max_participants", "discord.max_media_mb",
		"teams.media", "teams.media_scope", "teams.media_max_participants", "teams.max_media_mb",
	}
}

func EnrichmentControlKeys() []string {
	return []string{"people.enrichment.enabled", "people.enrichment.schedule", "people.enrichment.batch_size", "people.enrichment.lease_duration"}
}

type OperationalSettings struct {
	ETag           string                   `json:"etag"`
	PendingRestart bool                     `json:"pending_restart"`
	Groups         []generated.SettingGroup `json:"groups"`
	Settings       []generated.Setting      `json:"settings"`
}

type EnrichmentPolicy struct {
	ETag           string                                      `json:"etag"`
	PendingRestart bool                                        `json:"pending_restart"`
	Controls       []generated.Setting                         `json:"controls"`
	Providers      []generated.PersonEnrichmentProviderSetting `json:"providers"`
}

type EnrichmentPolicyChanges struct {
	Enabled               *bool     `json:"enabled,omitzero"`
	Mode                  *string   `json:"mode,omitzero"`
	Tier                  *string   `json:"tier,omitzero"`
	NumResults            *int64    `json:"num_results,omitzero"`
	AllowedIdentifiers    *[]string `json:"allowed_identifiers,omitzero"`
	TargetKeys            *[]string `json:"target_keys,omitzero"`
	AllowSensitiveTargets *bool     `json:"allow_sensitive_targets,omitzero"`
	RetentionPosture      *string   `json:"retention_posture,omitzero"`
	TrainingPosture       *string   `json:"training_posture,omitzero"`
	RefreshInterval       *string   `json:"refresh_interval,omitzero"`
	RequestTimeout        *string   `json:"request_timeout,omitzero"`
	PollInterval          *string   `json:"poll_interval,omitzero"`
	MaxJobAge             *string   `json:"max_job_age,omitzero"`
	MaxRetries            *int64    `json:"max_retries,omitzero"`
	MaxRequestsPerRun     *int64    `json:"max_requests_per_run,omitzero"`
	MaxRequestsPerDay     *int64    `json:"max_requests_per_day,omitzero"`
}

func typedSettingValueSchema() *jsonschema.Schema {
	values := map[string]*jsonschema.Schema{
		"string": stringSchema("String value"), "integer": {Type: mcpSchemaInteger}, "number": {Type: "number"}, "boolean": booleanSchema("Boolean value; false is explicit"), "strings": {Type: mcpSchemaArray, Items: stringSchema("Member")},
	}
	union := &jsonschema.Schema{OneOf: []*jsonschema.Schema{}}
	for _, key := range []string{"string", "integer", "number", "boolean", "strings"} {
		union.OneOf = append(union.OneOf, closedObject(map[string]*jsonschema.Schema{key: values[key]}, key))
	}
	return union
}

func settingUpdatesInput(keys []string) *jsonschema.Schema {
	union := typedSettingValueSchema()
	enum := make([]any, 0, len(keys))
	for _, key := range keys {
		enum = append(enum, key)
	}
	update := closedObject(map[string]*jsonschema.Schema{"key": {Type: mcpSchemaString, Enum: enum}, "value": union}, "key", "value")
	return closedObject(map[string]*jsonschema.Schema{"etag": stringSchema("Exact strong config ETag from the preceding read"), "updates": {Type: mcpSchemaArray, Items: update, MinItems: new(1), MaxItems: new(len(keys))}}, "etag", "updates")
}

func operationalSettingsOutputSchema[T any](field string) *jsonschema.Schema {
	schema := outputSchemaFor[T]()
	// Generated SettingValue stores its JSON union in private fields. Declare
	// its actual five wire variants, as the Slack policy projection does.
	schema.Properties[field].Items.Properties["value"] = typedSettingValueSchema()
	return schema
}

func settingsOperationalDefinitions() []operationalDefinition {
	changes := outputSchemaFor[EnrichmentPolicyChanges]()
	changes.AdditionalProperties = rejectAllSchema()
	changes.MinProperties = new(1)
	return []operationalDefinition{
		newOperationalDefinition("get_operational_settings", "Read operational settings and the exact config ETag. Credential hints are removed; host-only keys remain read-only to MCP.", OperationFamilySettings, closedObject(nil), operationalSettingsOutputSchema[OperationalSettings]("settings"), false, false),
		newOperationalDefinition("update_operational_settings", "Atomically update the fixed local operational key inventory with the caller's exact config ETag and approval. Preserve explicit false/zero; media policy changes affect future collection.", OperationFamilySettings, settingUpdatesInput(OperationalSettingKeys()), operationalSettingsOutputSchema[OperationalSettings]("settings"), true, false),
		newOperationalDefinition("get_enrichment_policy", "Read current named enrichment destinations, identifiers, target/privacy/request bounds and global controls without credential hints. No provider request.", OperationFamilyEnrichment, closedObject(nil), operationalSettingsOutputSchema[EnrichmentPolicy]("controls"), false, false),
		newOperationalDefinition("update_enrichment_controls", "Change only global enrichment enabled/schedule/batch_size/lease_duration with exact ETag and current policy disclosure. Enable may schedule provider work after restart.", OperationFamilyEnrichment, settingUpdatesInput(EnrichmentControlKeys()), operationalSettingsOutputSchema[EnrichmentPolicy]("controls"), true, false),
		newOperationalDefinition("update_enrichment_policy", "Update permitted policy fields on an existing named enrichment provider, preserving omitted fields and host-owned kind/destinations/credentials. Requires exact ETag and approval; no provider creation or destination change.", OperationFamilyEnrichment, closedObject(map[string]*jsonschema.Schema{"etag": stringSchema("Exact config ETag"), "name": stringSchema("Exact existing provider name"), "changes": changes}, "etag", "name", "changes"), operationalSettingsOutputSchema[EnrichmentPolicy]("controls"), true, false),
	}
}
