package mcp

import "github.com/google/jsonschema-go/jsonschema"

// EnrichmentPolicy contains only policy controls, never provider setup or credentials.
type EnrichmentPolicy struct {
	ETag           string                     `json:"etag"`
	PendingRestart bool                       `json:"pending_restart"`
	Providers      []EnrichmentProviderPolicy `json:"providers"`
}
type EnrichmentProviderPolicy struct {
	Name              string `json:"name"`
	Enabled           bool   `json:"enabled"`
	NumResults        *int64 `json:"num_results,omitempty"`
	MaxRetries        int64  `json:"max_retries"`
	MaxRequestsPerRun int64  `json:"max_requests_per_run"`
	MaxRequestsPerDay int64  `json:"max_requests_per_day"`
}

// OperationFamilyEnrichmentSettings must be independently enabled for writes.
const OperationFamilyEnrichmentSettings OperationFamily = "enrichment_settings"

func enrichmentSettingsDefinitions() []operationalDefinition {
	definitions := []operationalDefinition{
		newOperationalDefinition("get_enrichment_policy", "Read existing provider enabled state and numeric limits with the exact settings ETag. Provider setup and credentials are omitted.", OperationFamilyEnrichmentSettings, closedObject(nil), outputSchemaFor[EnrichmentPolicy](), false),
		newOperationalDefinition("update_enrichment_policy", "Change enabled state or numeric limits for an existing provider using the exact settings ETag. Requires approval; consent is managed separately and a restart may be required.", OperationFamilyEnrichmentSettings, closedObject(map[string]*jsonschema.Schema{
			"name": stringSchema("Exact existing provider name"), "etag": stringSchema("Exact ETag from get_enrichment_policy"),
			"enabled":              booleanSchema("Enable or disable this configured provider"),
			"num_results":          {Type: "integer", Minimum: new(float64)},
			"max_retries":          {Type: "integer", Minimum: new(float64)},
			"max_requests_per_run": {Type: "integer", Minimum: new(float64)},
			"max_requests_per_day": {Type: "integer", Minimum: new(float64)},
		}, "name", "etag"), outputSchemaFor[EnrichmentPolicy](), true),
	}
	definitions[1].definition.annotations.OpenWorldHint = new(true)
	return definitions
}
