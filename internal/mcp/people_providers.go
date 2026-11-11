package mcp

import "github.com/google/jsonschema-go/jsonschema"

type PeopleProviderProfile struct {
	Name             string   `json:"name"`
	Selected         bool     `json:"selected"`
	Protocol         string   `json:"protocol"`
	Model            string   `json:"model"`
	Checked          bool     `json:"checked"`
	ConsentActive    bool     `json:"consent_active"`
	AllowSensitive   bool     `json:"allow_sensitive"`
	AllowedSources   []string `json:"allowed_sources"`
	SourceSince      string   `json:"source_since"`
	SourceUntil      *string  `json:"source_until,omitempty"`
	RetentionPosture string   `json:"retention_posture"`
	TrainingPosture  string   `json:"training_posture"`
}
type PeopleProviderSettings struct {
	ETag              string                  `json:"etag"`
	ConfiguredEnabled bool                    `json:"configured_enabled"`
	ConfiguredName    *string                 `json:"configured_name,omitempty"`
	RunningEnabled    bool                    `json:"running_enabled"`
	RunningName       *string                 `json:"running_name,omitempty"`
	PendingRestart    bool                    `json:"pending_restart"`
	Profiles          []PeopleProviderProfile `json:"profiles"`
}

func peopleProviderOperationalDefinitions() []operationalDefinition {
	output := outputSchemaFor[PeopleProviderSettings]()
	return []operationalDefinition{
		newOperationalDefinition("get_people_provider_settings", "Read safe configured and running people inference policy and exact settings ETag.", OperationFamilyProviders, closedObject(nil), output, false),
		newOperationalDefinition("select_people_provider", "Select an already checked and consented configured provider using the exact ETag. Requires approval.", OperationFamilyProviders, closedObject(map[string]*jsonschema.Schema{"etag": stringSchema("Exact ETag from get_people_provider_settings"), "name": stringSchema("Configured provider name")}, "etag", "name"), output, true),
		newOperationalDefinition("disable_people_inference", "Disable inference and revoke active consent using the exact ETag. Operator re-consent is required to enable later. Requires approval.", OperationFamilyProviders, closedObject(map[string]*jsonschema.Schema{"etag": stringSchema("Exact ETag from get_people_provider_settings")}, "etag"), output, true),
	}
}
