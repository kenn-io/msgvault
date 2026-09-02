// Package peoplesweep owns the privacy and provider boundary for model-backed
// person-profile maintenance.
package peoplesweep

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	ProtocolOpenAIChat            Protocol = "openai_chat"
	ProtocolOpenAIResponses       Protocol = "openai_responses"
	ProtocolAnthropicMessages     Protocol = "anthropic_messages"
	ProtocolGoogleGenerateContent Protocol = "google_generate_content"
	ProtocolCodexAppServer        Protocol = "codex_app_server"

	OutputModeNativeJSONSchema OutputMode = "native_json_schema"
	OutputModeJSONObject       OutputMode = "json_object"
	OutputModePromptJSON       OutputMode = "prompt_json"

	AuthBearer       AuthScheme = "bearer"
	AuthXAPIKey      AuthScheme = "x_api_key"
	AuthGoogleAPIKey AuthScheme = "google_api_key"
	AuthNone         AuthScheme = "none"

	CredentialStored CredentialSource = "stored"
	CredentialEnv    CredentialSource = "env"
	CredentialNone   CredentialSource = "none"

	CodexExecutionBoundaryV1     = "codex-app-server-packet-only-v1"
	PacketRendererPolicyV1       = "person-sweep-packet-v1"
	reasoningModeProviderDefault = "provider_default"

	SourceConversationText  SourceClass = "conversation_text"
	SourceMeetingText       SourceClass = "meeting_text"
	SourceAttachmentCaption SourceClass = "attachment_caption"
	SourceAttachmentOCR     SourceClass = "attachment_ocr"
	SourceDocumentText      SourceClass = "document_text"
)

// Deprecated provider-kind aliases keep callers source-compatible while the
// runtime registry migrates to protocol identifiers.
const (
	ProviderOpenAICompatible = "openai_compatible"
	ProviderCodexAppServer   = "codex_app_server"
)

var disclosedPacketFieldsV1 = []string{
	"person_id",
	"program_identity",
	"catalog",
	"current_projection",
	"unresolved_claims",
	"seed_evidence",
	"retrieved_context",
}

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Protocol string
type OutputMode string
type AuthScheme string
type CredentialSource string

// SourceClass identifies one text-only archive lane that may contribute to a
// future evidence pack. It never identifies raw attachment or media bytes.
type SourceClass string

// ProviderSelection names the active profile. A legacy table is retained only
// until ApplyDefaults translates it into the named default profile.
type ProviderSelection struct {
	Name   string `toml:"-"` // MarshalTOML emits the selection as a scalar profile name.
	legacy *ProviderConfig
}

func (s *ProviderSelection) UnmarshalTOML(value any) error {
	switch typed := value.(type) {
	case string:
		s.Name = typed
		s.legacy = nil
		return nil
	case map[string]any:
		legacy, err := decodeLegacyProvider(typed)
		if err != nil {
			return err
		}
		s.Name = ""
		s.legacy = &legacy
		return nil
	default:
		return fmt.Errorf("[people.sweep] provider must be a profile name or legacy table, got %T", value)
	}
}

func (s *ProviderSelection) MarshalTOML() ([]byte, error) {
	return json.Marshal(s.Name)
}

// Config controls the model-backed people sweep. Disabled is the safe default.
//
//nolint:recvcheck // ApplyDefaults mutates while validation and profile construction do not.
type Config struct {
	Enabled              bool                      `toml:"enabled"`
	Schedule             string                    `toml:"schedule"`
	WorkBatchSize        int                       `toml:"work_batch_size"`
	ChangeBatchSize      int                       `toml:"change_batch_size"`
	HistoricalMessageCap int                       `toml:"historical_message_cap"`
	ContextPerTarget     int                       `toml:"context_per_target"`
	EvidenceMaxBytes     int                       `toml:"evidence_max_bytes"`
	EvidenceMaxItems     int                       `toml:"evidence_max_items"`
	LeaseDuration        time.Duration             `toml:"lease_duration"`
	BackstopInterval     time.Duration             `toml:"backstop_interval"`
	RetryBase            time.Duration             `toml:"retry_base"`
	RetryMax             time.Duration             `toml:"retry_max"`
	Budgets              BudgetConfig              `toml:"budgets"`
	Provider             ProviderSelection         `toml:"provider"`
	Providers            map[string]ProviderConfig `toml:"providers"`
}

// BudgetConfig caps hosted-inference usage. Costs are integer micro-USD so
// accounting stays exact without floating-point conversions.
type BudgetConfig struct {
	MaxRequestsPerPerson               int   `toml:"max_requests_per_person"`
	MaxInputTokensPerPerson            int64 `toml:"max_input_tokens_per_person"`
	MaxOutputTokensPerPerson           int64 `toml:"max_output_tokens_per_person"`
	MaxRequestsPerRun                  int   `toml:"max_requests_per_run"`
	MaxInputTokensPerRun               int64 `toml:"max_input_tokens_per_run"`
	MaxOutputTokensPerRun              int64 `toml:"max_output_tokens_per_run"`
	MaxEstimatedCostMicroUSDPerRun     int64 `toml:"max_estimated_cost_microusd_per_run"`
	MaxRequestsPerDay                  int   `toml:"max_requests_per_day"`
	MaxInputTokensPerDay               int64 `toml:"max_input_tokens_per_day"`
	MaxOutputTokensPerDay              int64 `toml:"max_output_tokens_per_day"`
	MaxEstimatedCostMicroUSDPerDay     int64 `toml:"max_estimated_cost_microusd_per_day"`
	InputCostMicroUSDPerMillionTokens  int64 `toml:"input_cost_microusd_per_million_tokens"`
	OutputCostMicroUSDPerMillionTokens int64 `toml:"output_cost_microusd_per_million_tokens"`
}

// ProviderConfig contains runtime settings and the exact outbound-data policy
// that must be consented before use.
type ProviderConfig struct {
	Protocol            Protocol         `toml:"protocol"`
	Endpoint            string           `toml:"endpoint"`
	Model               string           `toml:"model"`
	Auth                AuthScheme       `toml:"auth"`
	Credential          CredentialSource `toml:"credential"`
	CredentialEnv       string           `toml:"credential_env"`
	OutputMode          OutputMode       `toml:"output_mode"`
	TokenLimitParameter string           `toml:"token_limit_parameter"`
	ReasoningEffort     string           `toml:"reasoning_effort"`
	ReasoningMode       string           `toml:"reasoning_mode"`
	DriverVersion       string           `toml:"-"`
	RetentionPosture    string           `toml:"retention_posture"`
	TrainingPosture     string           `toml:"training_posture"`
	AllowedSources      []SourceClass    `toml:"allowed_sources"`
	SourceSince         string           `toml:"source_since"`
	SourceUntil         string           `toml:"source_until"`
	AllowSensitive      bool             `toml:"allow_sensitive"`
	Executable          string           `toml:"executable"`
	ExecutionBoundary   string           `toml:"execution_boundary"`
	RequestTimeout      time.Duration    `toml:"request_timeout"`
}

// ProviderProfile is one immutable, fingerprinted egress policy. PolicyJSON is
// canonical and intentionally excludes the credential value and request
// timeout.
type ProviderProfile struct {
	Fingerprint           string           `json:"fingerprint"`
	Protocol              Protocol         `json:"protocol"`
	Endpoint              string           `json:"endpoint"`
	Model                 string           `json:"model"`
	Auth                  AuthScheme       `json:"auth"`
	Credential            CredentialSource `json:"credential"`
	CredentialRef         string           `json:"credential_ref"`
	OutputMode            OutputMode       `json:"output_mode"`
	TokenLimitParameter   string           `json:"token_limit_parameter"`
	ReasoningEffort       string           `json:"reasoning_effort"`
	ReasoningMode         string           `json:"reasoning_mode"`
	DriverVersion         string           `json:"driver_version"`
	RetentionPosture      string           `json:"retention_posture"`
	TrainingPosture       string           `json:"training_posture"`
	AllowedSources        []SourceClass    `json:"allowed_sources"`
	SourceSince           string           `json:"source_since"`
	SourceUntil           string           `json:"source_until"`
	AllowSensitive        bool             `json:"allow_sensitive"`
	ExecutionBoundary     string           `json:"execution_boundary"`
	PacketRendererPolicy  string           `json:"packet_renderer_policy"`
	ProgramFingerprint    string           `json:"program_fingerprint"`
	DisclosedPacketFields []string         `json:"disclosed_packet_fields"`
	PolicyJSON            json.RawMessage  `json:"-"`
}

type providerPolicy struct {
	Protocol              Protocol         `json:"protocol"`
	Endpoint              string           `json:"endpoint"`
	Model                 string           `json:"model"`
	Auth                  AuthScheme       `json:"auth"`
	Credential            CredentialSource `json:"credential"`
	CredentialRef         string           `json:"credential_ref"`
	OutputMode            OutputMode       `json:"output_mode"`
	TokenLimitParameter   string           `json:"token_limit_parameter"`
	ReasoningEffort       string           `json:"reasoning_effort"`
	ReasoningMode         string           `json:"reasoning_mode"`
	DriverVersion         string           `json:"driver_version"`
	RetentionPosture      string           `json:"retention_posture"`
	TrainingPosture       string           `json:"training_posture"`
	AllowedSources        []SourceClass    `json:"allowed_sources"`
	SourceSince           string           `json:"source_since"`
	SourceUntil           string           `json:"source_until"`
	AllowSensitive        bool             `json:"allow_sensitive"`
	ExecutionBoundary     string           `json:"execution_boundary"`
	PacketRendererPolicy  string           `json:"packet_renderer_policy"`
	ProgramFingerprint    string           `json:"program_fingerprint"`
	DisclosedPacketFields []string         `json:"disclosed_packet_fields"`
}

type legacyProviderConfig struct {
	Kind              string        `toml:"kind"`
	Endpoint          string        `toml:"endpoint"`
	Model             string        `toml:"model"`
	APIKeyEnv         string        `toml:"api_key_env"`
	AllowAnonymous    bool          `toml:"allow_anonymous"`
	RetentionPosture  string        `toml:"retention_posture"`
	TrainingPosture   string        `toml:"training_posture"`
	AllowedSources    []SourceClass `toml:"allowed_sources"`
	SourceSince       string        `toml:"source_since"`
	SourceUntil       string        `toml:"source_until"`
	AllowSensitive    bool          `toml:"allow_sensitive"`
	ReasoningEffort   string        `toml:"reasoning_effort"`
	Executable        string        `toml:"executable"`
	ExecutionBoundary string        `toml:"execution_boundary"`
	RequestTimeout    time.Duration `toml:"request_timeout"`
}

func decodeLegacyProvider(table map[string]any) (ProviderConfig, error) {
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(table); err != nil {
		return ProviderConfig{}, fmt.Errorf("encode legacy [people.sweep.provider]: %w", err)
	}
	var legacy legacyProviderConfig
	if _, err := toml.Decode(encoded.String(), &legacy); err != nil {
		return ProviderConfig{}, fmt.Errorf("decode legacy [people.sweep.provider]: %w", err)
	}
	provider := ProviderConfig{
		Endpoint: legacy.Endpoint, Model: legacy.Model, CredentialEnv: legacy.APIKeyEnv,
		RetentionPosture: legacy.RetentionPosture, TrainingPosture: legacy.TrainingPosture,
		AllowedSources: slices.Clone(legacy.AllowedSources), SourceSince: legacy.SourceSince,
		SourceUntil: legacy.SourceUntil, AllowSensitive: legacy.AllowSensitive,
		ReasoningEffort: legacy.ReasoningEffort, Executable: legacy.Executable,
		ExecutionBoundary: legacy.ExecutionBoundary, RequestTimeout: legacy.RequestTimeout,
		OutputMode: OutputModeNativeJSONSchema,
	}
	switch legacy.Kind {
	case "", "openai_compatible":
		provider.Protocol = ProtocolOpenAIChat
		provider.TokenLimitParameter = "max_completion_tokens"
		if _, configured := table["endpoint"]; !configured {
			provider.Endpoint = "https://api.openai.com/v1"
		}
		if legacy.AllowAnonymous {
			if _, configured := table["api_key_env"]; configured && legacy.APIKeyEnv != "" {
				return ProviderConfig{}, errors.New("[people.sweep.provider] anonymous mode cannot also configure api_key_env")
			}
			provider.Auth = AuthNone
			provider.Credential = CredentialNone
			provider.CredentialEnv = ""
		} else {
			provider.Auth = AuthBearer
			provider.Credential = CredentialEnv
			if _, configured := table["api_key_env"]; !configured {
				provider.CredentialEnv = "OPENAI_API_KEY"
			}
		}
	case "codex_app_server":
		provider.Protocol = ProtocolCodexAppServer
		provider.Auth = AuthNone
		provider.Credential = CredentialNone
		provider.CredentialEnv = ""
	default:
		provider.Protocol = Protocol(legacy.Kind)
	}
	return provider, nil
}

// ApplyDefaults fills operational defaults without enabling inference.
func (c *Config) ApplyDefaults() {
	setDefaultString(&c.Schedule, "15 2 * * *")
	setDefaultInt(&c.WorkBatchSize, 25)
	setDefaultInt(&c.ChangeBatchSize, 256)
	setDefaultInt(&c.HistoricalMessageCap, 2_000)
	setDefaultInt(&c.ContextPerTarget, 8)
	setDefaultInt(&c.EvidenceMaxBytes, 131_072)
	setDefaultInt(&c.EvidenceMaxItems, 200)
	setDefaultDuration(&c.LeaseDuration, 15*time.Minute)
	setDefaultDuration(&c.BackstopInterval, 24*time.Hour)
	setDefaultDuration(&c.RetryBase, time.Minute)
	setDefaultDuration(&c.RetryMax, 6*time.Hour)
	setDefaultInt(&c.Budgets.MaxRequestsPerPerson, 4)
	setDefaultInt64(&c.Budgets.MaxInputTokensPerPerson, 200_000)
	setDefaultInt64(&c.Budgets.MaxOutputTokensPerPerson, 16_000)
	setDefaultInt(&c.Budgets.MaxRequestsPerRun, 100)
	setDefaultInt64(&c.Budgets.MaxInputTokensPerRun, 1_000_000)
	setDefaultInt64(&c.Budgets.MaxOutputTokensPerRun, 160_000)
	setDefaultInt(&c.Budgets.MaxRequestsPerDay, 500)
	setDefaultInt64(&c.Budgets.MaxInputTokensPerDay, 5_000_000)
	setDefaultInt64(&c.Budgets.MaxOutputTokensPerDay, 800_000)

	if c.Provider.legacy != nil && len(c.Providers) == 0 {
		c.Provider.Name = "default"
		c.Providers = map[string]ProviderConfig{"default": *c.Provider.legacy}
		c.Provider.legacy = nil
	} else if c.Provider.legacy == nil && c.Provider.Name == "" && len(c.Providers) == 0 {
		c.Provider.Name = "default"
		c.Providers = map[string]ProviderConfig{"default": defaultProviderConfig()}
	}
	for name, provider := range c.Providers {
		applyProviderDefaults(&provider)
		provider.AllowedSources = slices.Clone(provider.AllowedSources)
		c.Providers[name] = provider
	}
}

//nolint:gosec // This names the required environment variable; it is not a credential value.
func defaultProviderConfig() ProviderConfig {
	return ProviderConfig{
		Protocol: ProtocolOpenAIChat, Endpoint: "https://api.openai.com/v1",
		Auth: AuthBearer, Credential: CredentialEnv, CredentialEnv: "OPENAI_API_KEY",
		OutputMode: OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
	}
}

func applyProviderDefaults(provider *ProviderConfig) {
	setDefaultDuration(&provider.RequestTimeout, time.Minute)
	if provider.DriverVersion == "" {
		provider.DriverVersion = defaultDriverVersion(provider.Protocol)
	}
	if provider.Protocol == ProtocolCodexAppServer {
		setDefaultString(&provider.Executable, "codex")
		setDefaultString(&provider.ExecutionBoundary, CodexExecutionBoundaryV1)
	}
}

func defaultDriverVersion(protocol Protocol) string {
	switch protocol {
	case ProtocolOpenAIChat:
		return OpenAICompatibleProviderVersion
	case ProtocolOpenAIResponses:
		return "openai-responses-v1"
	case ProtocolAnthropicMessages:
		return "anthropic-messages-v1"
	case ProtocolGoogleGenerateContent:
		return "google-generate-content-v1"
	case ProtocolCodexAppServer:
		return CodexAppServerProviderVersion
	default:
		return ""
	}
}

// ActiveProviderConfig resolves the active profile by value so callers cannot
// retain a mutable map entry by pointer.
func (c Config) ActiveProviderConfig() (string, ProviderConfig, error) {
	if c.Provider.legacy != nil {
		if len(c.Providers) > 0 {
			return "", ProviderConfig{}, errors.New("legacy [people.sweep.provider] cannot be mixed with named [people.sweep.providers]")
		}
		return "", ProviderConfig{}, errors.New("legacy [people.sweep.provider] must be normalized with ApplyDefaults")
	}
	name := c.Provider.Name
	if name == "" {
		return "", ProviderConfig{}, errors.New("[people.sweep] provider profile name is required")
	}
	if err := ValidateProviderProfileName(name); err != nil {
		return "", ProviderConfig{}, err
	}
	provider, ok := c.Providers[name]
	if !ok {
		return "", ProviderConfig{}, fmt.Errorf("[people.sweep] provider %q is not defined", name)
	}
	provider.AllowedSources = slices.Clone(provider.AllowedSources)
	return name, provider, nil
}

// Validate rejects unsafe or ambiguous runtime configuration. An incomplete
// disabled policy is permitted, but any configured structural value must be
// well formed.
func (c Config) Validate() error {
	if c.Provider.legacy == nil {
		if c.Provider.Name != "" {
			if err := ValidateProviderProfileName(c.Provider.Name); err != nil {
				return err
			}
		}
		for name := range c.Providers {
			if err := ValidateProviderProfileName(name); err != nil {
				return err
			}
		}
	}
	if err := c.validateOperationalConfig(); err != nil {
		return err
	}
	_, provider, err := c.ActiveProviderConfig()
	if err != nil {
		return err
	}
	if provider.RequestTimeout <= 0 {
		return fmt.Errorf("invalid [people.sweep.provider] request_timeout %s: must be positive", provider.RequestTimeout)
	}
	return c.validateProvider(provider)
}

func (c Config) validateOperationalConfig() error {
	for _, value := range []struct {
		name  string
		value int
	}{
		{"work_batch_size", c.WorkBatchSize}, {"change_batch_size", c.ChangeBatchSize},
		{"historical_message_cap", c.HistoricalMessageCap}, {"context_per_target", c.ContextPerTarget},
		{"evidence_max_bytes", c.EvidenceMaxBytes}, {"evidence_max_items", c.EvidenceMaxItems},
		{"max_requests_per_person", c.Budgets.MaxRequestsPerPerson},
		{"max_requests_per_run", c.Budgets.MaxRequestsPerRun},
		{"max_requests_per_day", c.Budgets.MaxRequestsPerDay},
	} {
		if value.value <= 0 {
			return fmt.Errorf("invalid [people.sweep] %s: must be positive", value.name)
		}
	}
	for _, value := range []struct {
		name  string
		value int64
	}{
		{"max_input_tokens_per_person", c.Budgets.MaxInputTokensPerPerson},
		{"max_output_tokens_per_person", c.Budgets.MaxOutputTokensPerPerson},
		{"max_input_tokens_per_run", c.Budgets.MaxInputTokensPerRun},
		{"max_output_tokens_per_run", c.Budgets.MaxOutputTokensPerRun},
		{"max_input_tokens_per_day", c.Budgets.MaxInputTokensPerDay},
		{"max_output_tokens_per_day", c.Budgets.MaxOutputTokensPerDay},
	} {
		if value.value <= 0 {
			return fmt.Errorf("invalid [people.sweep.budgets] %s: must be positive", value.name)
		}
	}
	if c.LeaseDuration <= 0 || c.BackstopInterval <= 0 || c.RetryBase <= 0 || c.RetryMax <= 0 {
		return errors.New("invalid [people.sweep] lease, backstop, and retry durations must be positive")
	}
	if c.Budgets.MaxOutputTokensPerPerson < extractionMaxOutputTokens {
		return fmt.Errorf("invalid [people.sweep.budgets] max_output_tokens_per_person: must be at least %d", extractionMaxOutputTokens)
	}
	if c.Budgets.MaxEstimatedCostMicroUSDPerRun < 0 || c.Budgets.MaxEstimatedCostMicroUSDPerDay < 0 ||
		c.Budgets.InputCostMicroUSDPerMillionTokens < 0 || c.Budgets.OutputCostMicroUSDPerMillionTokens < 0 {
		return errors.New("invalid [people.sweep.budgets] cost values must not be negative")
	}
	if (c.Budgets.MaxEstimatedCostMicroUSDPerRun > 0 || c.Budgets.MaxEstimatedCostMicroUSDPerDay > 0) &&
		(c.Budgets.InputCostMicroUSDPerMillionTokens <= 0 || c.Budgets.OutputCostMicroUSDPerMillionTokens <= 0) {
		return errors.New("[people.sweep.budgets] cost prices are required when a positive cost cap is configured")
	}
	return nil
}

func (c Config) validateProvider(provider ProviderConfig) error {
	switch provider.Protocol {
	case ProtocolOpenAIChat:
		if err := requireOneOf(provider.TokenLimitParameter, "max_completion_tokens", "max_tokens"); err != nil {
			return err
		}
	case ProtocolOpenAIResponses, ProtocolAnthropicMessages, ProtocolGoogleGenerateContent:
		if err := requireEmpty(provider.TokenLimitParameter); err != nil {
			return err
		}
	case ProtocolCodexAppServer:
		if err := requireCodexIsolationFields(provider); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported people inference protocol %q", provider.Protocol)
	}
	if err := validateReasoning(provider); err != nil {
		return err
	}
	if provider.Protocol == ProtocolCodexAppServer {
		if !c.Enabled {
			return nil
		}
		if strings.TrimSpace(provider.Model) == "" || strings.TrimSpace(provider.ReasoningEffort) == "" {
			return errors.New("[people.sweep.provider] codex_app_server requires model and reasoning_effort")
		}
		return validateCommonEnabledPolicy(provider)
	}
	return c.validateHTTPProvider(provider)
}

func requireOneOf(value string, allowed ...string) error {
	if !slices.Contains(allowed, value) {
		return fmt.Errorf("[people.sweep.provider] token_limit_parameter %q is not supported", value)
	}
	return nil
}

func requireEmpty(value string) error {
	if value != "" {
		return errors.New("[people.sweep.provider] token_limit_parameter is only valid for openai_chat")
	}
	return nil
}

func requireCodexIsolationFields(provider ProviderConfig) error {
	if provider.Endpoint != "" || provider.Auth != AuthNone || provider.Credential != CredentialNone ||
		provider.CredentialEnv != "" {
		return errors.New("[people.sweep.provider] codex_app_server does not accept HTTP or credential fields")
	}
	if provider.ExecutionBoundary != CodexExecutionBoundaryV1 {
		return fmt.Errorf("invalid [people.sweep.provider] execution_boundary %q", provider.ExecutionBoundary)
	}
	return nil
}

func validateReasoning(provider ProviderConfig) error {
	if provider.ReasoningEffort != "" &&
		!slices.Contains([]string{"low", "medium", "high", "max"}, provider.ReasoningEffort) {
		return fmt.Errorf("invalid [people.sweep.provider] reasoning_effort %q", provider.ReasoningEffort)
	}
	if provider.ReasoningMode != "" &&
		!slices.Contains([]string{reasoningModeProviderDefault, "enabled", "disabled"}, provider.ReasoningMode) {
		return fmt.Errorf("invalid [people.sweep.provider] reasoning_mode %q", provider.ReasoningMode)
	}
	return nil
}

func (c Config) validateHTTPProvider(provider ProviderConfig) error {
	endpoint, loopback, err := validateEndpoint(provider.Endpoint)
	if err != nil {
		return err
	}
	if err := validateProtocolHTTPCapabilities(provider); err != nil {
		return err
	}
	if !slices.Contains([]OutputMode{OutputModeNativeJSONSchema, OutputModeJSONObject, OutputModePromptJSON}, provider.OutputMode) {
		return fmt.Errorf("invalid [people.sweep.provider] output_mode %q", provider.OutputMode)
	}
	if !slices.Contains([]AuthScheme{AuthBearer, AuthXAPIKey, AuthGoogleAPIKey, AuthNone}, provider.Auth) {
		return fmt.Errorf("invalid [people.sweep.provider] auth %q", provider.Auth)
	}
	if !slices.Contains([]CredentialSource{CredentialStored, CredentialEnv, CredentialNone}, provider.Credential) {
		return fmt.Errorf("invalid [people.sweep.provider] credential %q", provider.Credential)
	}
	if provider.Credential == CredentialEnv {
		if !environmentNamePattern.MatchString(provider.CredentialEnv) {
			return fmt.Errorf("invalid [people.sweep.provider] credential_env %q", provider.CredentialEnv)
		}
	} else if provider.CredentialEnv != "" {
		return errors.New("[people.sweep.provider] credential_env requires credential=env")
	}
	if provider.Auth == AuthNone || provider.Credential == CredentialNone {
		if provider.Auth != AuthNone || provider.Credential != CredentialNone {
			return errors.New("[people.sweep.provider] auth=none and credential=none must be configured together")
		}
		if !loopback {
			return errors.New("[people.sweep.provider] unauthenticated mode requires a loopback endpoint")
		}
	}
	if endpoint.Scheme == "http" && !loopback {
		return errors.New("[people.sweep.provider] remote endpoint must use HTTPS")
	}
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(provider.Model) == "" {
		return errors.New("[people.sweep.provider] model is required when people sweep is enabled")
	}
	return validateCommonEnabledPolicy(provider)
}

func validateCommonEnabledPolicy(provider ProviderConfig) error {
	if err := validatePosture("retention", provider.RetentionPosture); err != nil {
		return err
	}
	if err := validatePosture("training", provider.TrainingPosture); err != nil {
		return err
	}
	if err := validateSources(provider.AllowedSources); err != nil {
		return err
	}
	if err := validateDate("source_since", provider.SourceSince, false); err != nil {
		return err
	}
	if err := validateDate("source_until", provider.SourceUntil, true); err != nil {
		return err
	}
	if provider.SourceUntil != "" && provider.SourceUntil < provider.SourceSince {
		return errors.New("[people.sweep.provider] source_until is before source_since")
	}
	return nil
}

// Profile returns the canonical immutable policy for enabled configuration.
func (c Config) Profile() (ProviderProfile, error) {
	if !c.Enabled {
		return ProviderProfile{}, errors.New("people sweep provider is disabled")
	}
	if err := c.Validate(); err != nil {
		return ProviderProfile{}, err
	}
	name, provider, err := c.ActiveProviderConfig()
	if err != nil {
		return ProviderProfile{}, err
	}
	endpoint := (*url.URL)(nil)
	if provider.Protocol != ProtocolCodexAppServer {
		endpoint, _, err = validateEndpoint(provider.Endpoint)
		if err != nil {
			return ProviderProfile{}, err
		}
	}
	credentialRef := ""
	switch provider.Credential {
	case CredentialEnv:
		credentialRef = provider.CredentialEnv
	case CredentialStored:
		credentialRef = name
	case CredentialNone:
		credentialRef = ""
	}
	sources := slices.Clone(provider.AllowedSources)
	slices.Sort(sources)
	policy := providerPolicy{
		Protocol: provider.Protocol, Model: strings.TrimSpace(provider.Model),
		Auth: provider.Auth, Credential: provider.Credential, CredentialRef: credentialRef,
		OutputMode: provider.OutputMode, TokenLimitParameter: provider.TokenLimitParameter,
		ReasoningEffort: strings.TrimSpace(provider.ReasoningEffort), ReasoningMode: provider.ReasoningMode,
		DriverVersion: provider.DriverVersion, RetentionPosture: strings.TrimSpace(provider.RetentionPosture),
		TrainingPosture: strings.TrimSpace(provider.TrainingPosture), AllowedSources: sources,
		SourceSince: provider.SourceSince, SourceUntil: provider.SourceUntil,
		AllowSensitive: provider.AllowSensitive, ExecutionBoundary: provider.ExecutionBoundary,
		PacketRendererPolicy: PacketRendererPolicyV1, ProgramFingerprint: ProgramFingerprint(),
		DisclosedPacketFields: slices.Clone(disclosedPacketFieldsV1),
	}
	if endpoint != nil {
		policy.Endpoint = canonicalEndpoint(endpoint)
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return ProviderProfile{}, fmt.Errorf("encode people inference provider policy: %w", err)
	}
	digest := sha256.Sum256(policyJSON)
	return ProviderProfile{
		Fingerprint: hex.EncodeToString(digest[:]), Protocol: policy.Protocol,
		Endpoint: policy.Endpoint, Model: policy.Model, Auth: policy.Auth,
		Credential: policy.Credential, CredentialRef: policy.CredentialRef,
		OutputMode: policy.OutputMode, TokenLimitParameter: policy.TokenLimitParameter,
		ReasoningEffort: policy.ReasoningEffort, ReasoningMode: policy.ReasoningMode,
		DriverVersion: policy.DriverVersion, RetentionPosture: policy.RetentionPosture,
		TrainingPosture: policy.TrainingPosture, AllowedSources: slices.Clone(policy.AllowedSources),
		SourceSince: policy.SourceSince, SourceUntil: policy.SourceUntil,
		AllowSensitive: policy.AllowSensitive, ExecutionBoundary: policy.ExecutionBoundary,
		PacketRendererPolicy: policy.PacketRendererPolicy, ProgramFingerprint: policy.ProgramFingerprint,
		DisclosedPacketFields: slices.Clone(policy.DisclosedPacketFields), PolicyJSON: policyJSON,
	}, nil
}

// Validate proves the profile fields, canonical policy bytes, and fingerprint
// still describe exactly the same policy.
func (p ProviderProfile) Validate() error {
	name := "profile"
	provider := ProviderConfig{
		Protocol: p.Protocol, Endpoint: p.Endpoint, Model: p.Model, Auth: p.Auth,
		Credential: p.Credential, OutputMode: p.OutputMode,
		TokenLimitParameter: p.TokenLimitParameter, ReasoningEffort: p.ReasoningEffort,
		ReasoningMode: p.ReasoningMode, DriverVersion: p.DriverVersion,
		RetentionPosture: p.RetentionPosture, TrainingPosture: p.TrainingPosture,
		AllowedSources: slices.Clone(p.AllowedSources), SourceSince: p.SourceSince,
		SourceUntil: p.SourceUntil, AllowSensitive: p.AllowSensitive,
		ExecutionBoundary: p.ExecutionBoundary, RequestTimeout: time.Second,
	}
	switch p.Credential {
	case CredentialEnv:
		provider.CredentialEnv = p.CredentialRef
	case CredentialStored:
		name = p.CredentialRef
	case CredentialNone:
		provider.CredentialEnv = ""
	}
	config := Config{
		Enabled: true, Provider: ProviderSelection{Name: name},
		Providers: map[string]ProviderConfig{name: provider},
	}
	config.ApplyDefaults()
	want, err := config.Profile()
	if err != nil {
		return err
	}
	if p.Fingerprint != want.Fingerprint {
		return errors.New("people inference provider profile fingerprint does not match policy")
	}
	if !bytes.Equal(p.PolicyJSON, want.PolicyJSON) {
		return errors.New("people inference provider profile policy is not canonical")
	}
	if p.Protocol != want.Protocol || p.Endpoint != want.Endpoint || p.Model != want.Model ||
		p.Auth != want.Auth || p.Credential != want.Credential || p.CredentialRef != want.CredentialRef ||
		p.OutputMode != want.OutputMode || p.TokenLimitParameter != want.TokenLimitParameter ||
		p.ReasoningEffort != want.ReasoningEffort || p.ReasoningMode != want.ReasoningMode ||
		p.DriverVersion != want.DriverVersion || p.RetentionPosture != want.RetentionPosture ||
		p.TrainingPosture != want.TrainingPosture || !slices.Equal(p.AllowedSources, want.AllowedSources) ||
		p.SourceSince != want.SourceSince || p.SourceUntil != want.SourceUntil ||
		p.AllowSensitive != want.AllowSensitive || p.ExecutionBoundary != want.ExecutionBoundary ||
		p.PacketRendererPolicy != want.PacketRendererPolicy ||
		p.ProgramFingerprint != want.ProgramFingerprint ||
		!slices.Equal(p.DisclosedPacketFields, want.DisclosedPacketFields) {
		return errors.New("people inference provider profile fields are not canonical")
	}
	return nil
}

func setDefaultString(target *string, value string) {
	if *target == "" {
		*target = value
	}
}

func setDefaultInt(target *int, value int) {
	if *target == 0 {
		*target = value
	}
}

func setDefaultInt64(target *int64, value int64) {
	if *target == 0 {
		*target = value
	}
}

func setDefaultDuration(target *time.Duration, value time.Duration) {
	if *target == 0 {
		*target = value
	}
}

// validateProtocolHTTPCapabilities mirrors, at config-validation time, the
// per-protocol contracts each HTTP driver enforces again at Prepare as
// defense in depth. Without this centralization, a protocol-incompatible
// profile (for example bearer auth for anthropic_messages) passes startup
// validation and then fails every scheduled sweep during preparation.
func validateProtocolHTTPCapabilities(provider ProviderConfig) error {
	switch provider.Protocol {
	case ProtocolOpenAIChat:
		// Every configured auth scheme, output mode, and reasoning setting is
		// representable on the OpenAI Chat wire format.
		return nil
	case ProtocolOpenAIResponses:
		if provider.ReasoningMode != "" && provider.ReasoningMode != reasoningModeProviderDefault {
			return fmt.Errorf(
				"[people.sweep.provider] reasoning_mode %q is not supported by openai_responses",
				provider.ReasoningMode)
		}
		return nil
	case ProtocolAnthropicMessages:
		if provider.Auth != AuthXAPIKey {
			return fmt.Errorf("[people.sweep.provider] anthropic_messages requires auth %q", AuthXAPIKey)
		}
		if provider.OutputMode == OutputModeJSONObject {
			return fmt.Errorf(
				"[people.sweep.provider] output_mode %q is not supported by anthropic_messages",
				OutputModeJSONObject)
		}
		return validateProviderDefaultReasoning(provider, "anthropic_messages")
	case ProtocolGoogleGenerateContent:
		if provider.Auth != AuthGoogleAPIKey {
			return fmt.Errorf("[people.sweep.provider] google_generate_content requires auth %q", AuthGoogleAPIKey)
		}
		if provider.OutputMode == OutputModeJSONObject {
			return fmt.Errorf(
				"[people.sweep.provider] output_mode %q is not supported by google_generate_content",
				OutputModeJSONObject)
		}
		return validateProviderDefaultReasoning(provider, "google_generate_content")
	default:
		return nil
	}
}

// validateProviderDefaultReasoning rejects reasoning settings the protocol's
// driver cannot represent on its wire format.
func validateProviderDefaultReasoning(provider ProviderConfig, protocol string) error {
	if provider.ReasoningEffort != "" ||
		(provider.ReasoningMode != "" && provider.ReasoningMode != reasoningModeProviderDefault) {
		return fmt.Errorf("[people.sweep.provider] reasoning settings are not supported by %s", protocol)
	}
	return nil
}

func validateEndpoint(raw string) (*url.URL, bool, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, false, fmt.Errorf("invalid [people.sweep.provider] endpoint %q", raw)
	}
	if parsed.User != nil {
		return nil, false, errors.New("[people.sweep.provider] endpoint must not contain credentials")
	}
	if parsed.RawQuery != "" {
		return nil, false, errors.New("[people.sweep.provider] endpoint must not contain a query")
	}
	if parsed.Fragment != "" {
		return nil, false, errors.New("[people.sweep.provider] endpoint must not contain a fragment")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, false, errors.New("[people.sweep.provider] endpoint must use HTTPS or loopback HTTP")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if parsed.Scheme == "http" && !loopback {
		return nil, false, errors.New("[people.sweep.provider] remote endpoint must use HTTPS")
	}
	return parsed, loopback, nil
}

func canonicalEndpoint(endpoint *url.URL) string {
	canonical := *endpoint
	canonical.Path = strings.TrimRight(canonical.Path, "/")
	return canonical.String()
}

func validatePosture(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "unknown") {
		return fmt.Errorf("[people.sweep.provider] %s_posture must be explicit", name)
	}
	return nil
}

func validateSources(sources []SourceClass) error {
	if len(sources) == 0 {
		return errors.New("[people.sweep.provider] allowed_sources must not be empty")
	}
	seen := make(map[SourceClass]struct{}, len(sources))
	for _, source := range sources {
		switch source {
		case SourceConversationText, SourceMeetingText, SourceDocumentText:
		case SourceAttachmentCaption, SourceAttachmentOCR:
			return fmt.Errorf("[people.sweep.provider] allowed_sources %q is not yet supported", source)
		default:
			return fmt.Errorf("[people.sweep.provider] allowed_sources contains %q", source)
		}
		if _, exists := seen[source]; exists {
			return fmt.Errorf("[people.sweep.provider] allowed_sources contains duplicate %q", source)
		}
		seen[source] = struct{}{}
	}
	return nil
}

func validateDate(name, value string, optional bool) error {
	if value == "" && optional {
		return nil
	}
	parsed, err := time.Parse(time.DateOnly, value)
	if err != nil || parsed.Format(time.DateOnly) != value {
		return fmt.Errorf("[people.sweep.provider] %s must be YYYY-MM-DD", name)
	}
	return nil
}
