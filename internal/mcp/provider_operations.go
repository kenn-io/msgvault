package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type PeopleProviderProfile struct {
	Name                 string   `json:"name"`
	Selected             bool     `json:"selected"`
	PresetID             string   `json:"preset_id,omitempty"`
	Protocol             string   `json:"protocol"`
	Endpoint             string   `json:"endpoint,omitempty"`
	Model                string   `json:"model"`
	CredentialSource     string   `json:"credential_source"`
	CredentialConfigured bool     `json:"credential_configured"`
	Checked              bool     `json:"checked"`
	ConsentActive        bool     `json:"consent_active"`
	OutputMode           string   `json:"output_mode"`
	RetentionPosture     string   `json:"retention_posture"`
	TrainingPosture      string   `json:"training_posture"`
	AllowedSources       []string `json:"allowed_sources"`
	SourceSince          string   `json:"source_since"`
	SourceUntil          string   `json:"source_until,omitempty"`
	AllowSensitive       bool     `json:"allow_sensitive"`
	Fingerprint          string   `json:"fingerprint,omitempty"`
	ReasoningEffort      string   `json:"reasoning_effort,omitempty"`
	ReasoningMode        string   `json:"reasoning_mode,omitempty"`
	RequestTimeout       string   `json:"request_timeout"`
}

type PeopleProviderSettings struct {
	ETag                       string                  `json:"etag"`
	StoredCredentialsSupported bool                    `json:"stored_credentials_supported"`
	Profiles                   []PeopleProviderProfile `json:"profiles"`
	ConfiguredName             string                  `json:"configured_name,omitempty"`
	ConfiguredEnabled          bool                    `json:"configured_enabled"`
	ConfiguredFingerprint      string                  `json:"configured_fingerprint,omitempty"`
	RunningName                string                  `json:"running_name,omitempty"`
	RunningEnabled             bool                    `json:"running_enabled"`
	RunningFingerprint         string                  `json:"running_fingerprint,omitempty"`
	PendingRestart             bool                    `json:"pending_restart"`
}

type PeopleProviderDisclosure struct {
	PeopleProviderProfile

	Auth                  string   `json:"auth"`
	DriverVersion         string   `json:"driver_version"`
	TokenLimitParameter   string   `json:"token_limit_parameter,omitempty"`
	ExecutionBoundary     string   `json:"execution_boundary,omitempty"`
	PacketRendererPolicy  string   `json:"packet_renderer_policy,omitempty"`
	ProgramFingerprint    string   `json:"program_fingerprint,omitempty"`
	DisclosedPacketFields []string `json:"disclosed_packet_fields,omitempty"`
}

type PeopleProviderStatus struct {
	Settings            PeopleProviderSettings             `json:"settings"`
	Policy              PeopleProviderDisclosure           `json:"policy"`
	Check               *store.PersonInferenceCheck        `json:"check,omitempty"`
	Consent             store.PersonInferenceConsentStatus `json:"consent"`
	StaleProgramCheck   bool                               `json:"stale_program_check"`
	StaleProgramConsent bool                               `json:"stale_program_consent"`
	Isolation           *PeopleProviderIsolation           `json:"isolation,omitempty"`
}
type PeopleProviderIsolation struct {
	Available         bool   `json:"available"`
	ExecutionBoundary string `json:"execution_boundary"`
}
type PeopleProviderUsage struct {
	Requests              int   `json:"requests"`
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	EstimatedCostMicroUSD int64 `json:"estimated_cost_microusd"`
}
type PeopleProviderHistoryRun struct {
	Kind            string              `json:"kind"`
	Mode            string              `json:"mode"`
	Status          string              `json:"status"`
	Attempts        int                 `json:"attempts"`
	Successes       int                 `json:"successes"`
	Failures        int                 `json:"failures"`
	ProjectedWrites int                 `json:"projected_writes"`
	Usage           PeopleProviderUsage `json:"usage"`
}
type PeopleProviderHistoryAttempt struct {
	PersonID        int64               `json:"person_id"`
	Status          string              `json:"status"`
	FailureClass    string              `json:"failure_class,omitempty"`
	SeedCount       int                 `json:"seed_count"`
	ContextCount    int                 `json:"context_count"`
	ClaimCount      int                 `json:"claim_count"`
	DecisionCount   int                 `json:"decision_count"`
	ProjectedWrites int                 `json:"projected_writes"`
	Usage           PeopleProviderUsage `json:"usage"`
	LatencyMS       int64               `json:"latency_ms"`
}
type PeopleProviderHistory struct {
	Runs     []PeopleProviderHistoryRun     `json:"runs"`
	Attempts []PeopleProviderHistoryAttempt `json:"attempts"`
}

func providerOperationalDefinitions() []operationalDefinition {
	settings := outputSchemaFor[PeopleProviderSettings]()
	named := func(properties map[string]*jsonschema.Schema, extra ...string) *jsonschema.Schema {
		if properties == nil {
			properties = map[string]*jsonschema.Schema{}
		}
		properties["name"] = stringSchema("Exact configured provider profile name")
		properties["etag"] = stringSchema("Exact strong ETag from get_people_provider_settings")
		return closedObject(properties, append([]string{"name", "etag"}, extra...)...)
	}
	policy := map[string]*jsonschema.Schema{
		"model": stringSchema("Provider model"), "retention_posture": stringSchema("Operator-confirmed retention policy"), "training_posture": stringSchema("Operator-confirmed training policy"),
		"allowed_sources": {Type: mcpSchemaArray, Items: stringSchema("conversation_text, meeting_text or document_text"), MinItems: new(1)},
		"source_since":    stringSchema("Inclusive lower source date"), "source_until": stringSchema("Upper source date; empty clears it"), "allow_sensitive": booleanSchema("Permit sensitive source packets"),
		"reasoning_effort": stringSchema("Existing provider reasoning effort; empty clears it"), "reasoning_mode": stringSchema("Existing provider reasoning mode; empty clears it"), "request_timeout": stringSchema("Positive Go duration for provider requests"),
	}
	preset := map[string]*jsonschema.Schema{}
	for _, key := range []string{"model", "retention_posture", "training_posture", "allowed_sources", "source_since", "source_until", "allow_sensitive"} {
		preset[key] = policy[key]
	}
	preset["preset_id"] = &jsonschema.Schema{Type: "string", Enum: []any{"openai", "openrouter", "venice"}}
	return []operationalDefinition{
		newOperationalDefinition("get_people_provider_settings", "Read configured and running provider policies, credential readiness, checks, consent, restart state and exact config ETag. Credential values and environment names are excluded.", OperationFamilyProviders, closedObject(nil), settings, false, false),
		newOperationalDefinition("get_people_provider_status", "Read one configured provider's exact disclosure, synthetic check and consent state. This does not contact the provider.", OperationFamilyProviders, closedObject(map[string]*jsonschema.Schema{"name": stringSchema("Optional configured profile; defaults to selected")}), outputSchemaFor[PeopleProviderStatus](), false, false),
		newOperationalDefinition("list_people_provider_history", "Read redacted provider sweep runs and attempts, optionally scoped to a provider and durable person.", OperationFamilyProviders, closedObject(map[string]*jsonschema.Schema{"name": stringSchema("Optional configured provider"), "person_id": safeIDSchema("Optional durable person ID"), "limit": {Type: mcpSchemaInteger, Minimum: new(float64(1)), Maximum: new(float64(200))}}), outputSchemaFor[PeopleProviderHistory](), false, false),
		newOperationalDefinition("create_people_provider_preset", "Create a named fixed-endpoint preset policy. Credential enrollment is separate host/Web setup. Requires exact ETag and approval.", OperationFamilyProviders, named(preset, "preset_id", "model", "retention_posture", "training_posture", "allowed_sources", "source_since", "allow_sensitive"), settings, true, false),
		newOperationalDefinition("check_people_provider", "Send fixed synthetic input to the configured provider; this may incur cost. Requires exact ETag and approval.", OperationFamilyProviders, named(nil), outputSchemaFor[generated.PeopleInferenceCheckResponse](), true, false),
		newOperationalDefinition("consent_people_provider", "Grant consent for the exact checked policy fingerprint. Requires exact ETag and explicit per-call approval.", OperationFamilyProviders, named(map[string]*jsonschema.Schema{"fingerprint": stringSchema("Exact current checked policy fingerprint")}, "fingerprint"), settings, true, false),
		newOperationalDefinition("select_people_provider", "Select an existing provider policy for the next daemon startup. Requires exact ETag and approval; selection does not restart the daemon.", OperationFamilyProviders, named(nil), settings, true, false),
		newOperationalDefinition("revoke_people_provider_consent", "Revoke consent for the named configured provider policy. Requires exact ETag and approval.", OperationFamilyProviders, named(nil), settings, true, false),
		newOperationalDefinition("disable_people_inference", "Disable configured people inference using the exact ETag. Requires approval and reports restart state.", OperationFamilyProviders, closedObject(map[string]*jsonschema.Schema{"etag": stringSchema("Exact config ETag")}, "etag"), settings, true, false),
		newOperationalDefinition("update_people_provider_policy", "Update existing non-secret policy while preserving its credential source. Negotiation and a synthetic check may incur cost. Consent is revoked; a failed check attempts exact-file rollback and never restores consent. Requires exact ETag and approval.", OperationFamilyProviders, named(policy), settings, true, false),
		newOperationalDefinition("remove_people_provider", "Remove an existing named provider policy and its stored credential according to daemon enrollment semantics. Requires exact ETag and approval.", OperationFamilyProviders, named(nil), settings, true, false),
	}
}
