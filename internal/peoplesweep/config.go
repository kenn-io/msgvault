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
)

const (
	ProviderOpenAICompatible = "openai_compatible"
	ProviderCodexAppServer   = "codex_app_server"
	ProviderOrcaRouter       = "orcarouter"

	CodexExecutionBoundaryV1 = "codex-app-server-packet-only-v1"
	PacketRendererPolicyV1   = "person-sweep-packet-v1"

	// OrcaRouter defaults for the [people.sweep.provider] block. The
	// gateway serves the OpenAI-compatible Chat Completions contract, so
	// the named provider shares the OpenAI-compatible transport.
	OrcaRouterDefaultEndpoint  = "https://api.orcarouter.ai/v1"
	OrcaRouterDefaultModel     = "orcarouter/auto"
	OrcaRouterDefaultAPIKeyEnv = "ORCAROUTER_API_KEY"
	OrcaRouterSignupURL        = "https://www.orcarouter.ai"

	SourceConversationText  SourceClass = "conversation_text"
	SourceMeetingText       SourceClass = "meeting_text"
	SourceAttachmentCaption SourceClass = "attachment_caption"
	SourceAttachmentOCR     SourceClass = "attachment_ocr"
	SourceDocumentText      SourceClass = "document_text"
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

// SourceClass identifies one text-only archive lane that may contribute to a
// future evidence pack. It never identifies raw attachment or media bytes.
type SourceClass string

// PeopleConfig groups configuration for durable people.
type PeopleConfig struct {
	Sweep Config `toml:"sweep"`
}

// Config controls the model-backed people sweep. Disabled is the safe default.
//
//nolint:recvcheck // ApplyDefaults mutates while validation and profile construction do not.
type Config struct {
	Enabled              bool           `toml:"enabled"`
	Schedule             string         `toml:"schedule"`
	WorkBatchSize        int            `toml:"work_batch_size"`
	ChangeBatchSize      int            `toml:"change_batch_size"`
	HistoricalMessageCap int            `toml:"historical_message_cap"`
	ContextPerTarget     int            `toml:"context_per_target"`
	EvidenceMaxBytes     int            `toml:"evidence_max_bytes"`
	EvidenceMaxItems     int            `toml:"evidence_max_items"`
	LeaseDuration        time.Duration  `toml:"lease_duration"`
	BackstopInterval     time.Duration  `toml:"backstop_interval"`
	RetryBase            time.Duration  `toml:"retry_base"`
	RetryMax             time.Duration  `toml:"retry_max"`
	Budgets              BudgetConfig   `toml:"budgets"`
	Provider             ProviderConfig `toml:"provider"`
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

// ProviderConfig contains both runtime settings and the exact outbound-data
// policy that must be consented before use.
type ProviderConfig struct {
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

// ProviderProfile is one immutable, fingerprinted egress policy. PolicyJSON is
// canonical and intentionally excludes the credential value and request
// timeout.
type ProviderProfile struct {
	Fingerprint           string          `json:"fingerprint"`
	Kind                  string          `json:"kind"`
	Endpoint              string          `json:"endpoint"`
	Model                 string          `json:"model"`
	APIKeyEnv             string          `json:"api_key_env"`
	AllowAnonymous        bool            `json:"allow_anonymous"`
	RetentionPosture      string          `json:"retention_posture"`
	TrainingPosture       string          `json:"training_posture"`
	AllowedSources        []SourceClass   `json:"allowed_sources"`
	SourceSince           string          `json:"source_since"`
	SourceUntil           string          `json:"source_until"`
	AllowSensitive        bool            `json:"allow_sensitive"`
	ReasoningEffort       string          `json:"reasoning_effort"`
	ExecutionBoundary     string          `json:"execution_boundary"`
	PacketRendererPolicy  string          `json:"packet_renderer_policy"`
	ProgramFingerprint    string          `json:"program_fingerprint"`
	DisclosedPacketFields []string        `json:"disclosed_packet_fields"`
	PolicyJSON            json.RawMessage `json:"-"`
}

type providerPolicy struct {
	Kind                  string        `json:"kind"`
	Endpoint              string        `json:"endpoint"`
	Model                 string        `json:"model"`
	APIKeyEnv             string        `json:"api_key_env"`
	AllowAnonymous        bool          `json:"allow_anonymous"`
	RetentionPosture      string        `json:"retention_posture"`
	TrainingPosture       string        `json:"training_posture"`
	AllowedSources        []SourceClass `json:"allowed_sources"`
	SourceSince           string        `json:"source_since"`
	SourceUntil           string        `json:"source_until"`
	AllowSensitive        bool          `json:"allow_sensitive"`
	ReasoningEffort       string        `json:"reasoning_effort"`
	ExecutionBoundary     string        `json:"execution_boundary"`
	PacketRendererPolicy  string        `json:"packet_renderer_policy"`
	ProgramFingerprint    string        `json:"program_fingerprint"`
	DisclosedPacketFields []string      `json:"disclosed_packet_fields"`
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
	if c.Provider.Kind == "" {
		c.Provider.Kind = ProviderOpenAICompatible
	}
	if c.Provider.Kind == ProviderOpenAICompatible {
		setDefaultString(&c.Provider.Endpoint, "https://api.openai.com/v1")
		if c.Provider.APIKeyEnv == "" && !c.Provider.AllowAnonymous {
			c.Provider.APIKeyEnv = "OPENAI_API_KEY"
		}
	}
	if c.Provider.Kind == ProviderOrcaRouter {
		// Selecting the gateway by name fills in its defaults so an
		// operator can point people-sweep at OrcaRouter with a minimal
		// block. Explicit settings always win.
		if c.Provider.Endpoint == "" {
			c.Provider.Endpoint = OrcaRouterDefaultEndpoint
		}
		if c.Provider.Model == "" {
			c.Provider.Model = OrcaRouterDefaultModel
		}
		if c.Provider.APIKeyEnv == "" && !c.Provider.AllowAnonymous {
			c.Provider.APIKeyEnv = OrcaRouterDefaultAPIKeyEnv
		}
	}
	if c.Provider.Kind == ProviderCodexAppServer {
		setDefaultString(&c.Provider.Executable, "codex")
		setDefaultString(&c.Provider.ExecutionBoundary, CodexExecutionBoundaryV1)
	}
	setDefaultDuration(&c.Provider.RequestTimeout, time.Minute)
}

// Validate rejects unsafe or ambiguous runtime configuration. An incomplete
// disabled policy is permitted, but any configured structural value must be
// well formed.
func (c Config) Validate() error {
	if err := c.validateOperationalConfig(); err != nil {
		return err
	}
	if c.Provider.RequestTimeout <= 0 {
		return fmt.Errorf("invalid [people.sweep.provider] request_timeout %s: must be positive",
			c.Provider.RequestTimeout)
	}
	switch c.Provider.Kind {
	case ProviderOpenAICompatible, ProviderOrcaRouter:
		return c.validateOpenAICompatible()
	case ProviderCodexAppServer:
		return c.validateCodexAppServer()
	default:
		return fmt.Errorf("invalid [people.sweep.provider] kind %q", c.Provider.Kind)
	}
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
		return fmt.Errorf("invalid [people.sweep.budgets] max_output_tokens_per_person: must be at least %d",
			extractionMaxOutputTokens)
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

func (c Config) validateOpenAICompatible() error {
	if c.Provider.ReasoningEffort != "" || c.Provider.ExecutionBoundary != "" {
		return errors.New("[people.sweep.provider] Codex-only fields are not allowed for openai_compatible")
	}
	endpoint, loopback, err := validateEndpoint(c.Provider.Endpoint)
	if err != nil {
		return err
	}
	if c.Provider.APIKeyEnv != "" && !environmentNamePattern.MatchString(c.Provider.APIKeyEnv) {
		return fmt.Errorf("invalid [people.sweep.provider] api_key_env %q", c.Provider.APIKeyEnv)
	}
	if c.Provider.AllowAnonymous && c.Provider.APIKeyEnv != "" {
		return errors.New("[people.sweep.provider] anonymous mode cannot also configure api_key_env")
	}
	if endpoint.Scheme == "http" && (!loopback || !c.Provider.AllowAnonymous || c.Provider.APIKeyEnv != "") {
		return errors.New("[people.sweep.provider] HTTP requires anonymous loopback mode without api_key_env")
	}
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Provider.Model) == "" {
		return errors.New("[people.sweep.provider] model is required when people sweep is enabled")
	}
	if c.Provider.AllowAnonymous {
		if !loopback {
			return errors.New("[people.sweep.provider] anonymous mode requires a loopback endpoint")
		}
	} else if c.Provider.APIKeyEnv == "" {
		return errors.New("[people.sweep.provider] api_key_env is required unless anonymous loopback mode is enabled")
	}
	if endpoint.Scheme == "http" && !loopback {
		return errors.New("[people.sweep.provider] remote endpoint must use HTTPS")
	}
	return c.validateCommonEnabledPolicy()
}

func (c Config) validateCodexAppServer() error {
	if c.Provider.Endpoint != "" || c.Provider.APIKeyEnv != "" || c.Provider.AllowAnonymous {
		return errors.New("[people.sweep.provider] codex_app_server does not accept endpoint, api_key_env, or anonymous mode")
	}
	if c.Provider.ExecutionBoundary != CodexExecutionBoundaryV1 {
		return fmt.Errorf("invalid [people.sweep.provider] execution_boundary %q", c.Provider.ExecutionBoundary)
	}
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Provider.Model) == "" || strings.TrimSpace(c.Provider.ReasoningEffort) == "" {
		return errors.New("[people.sweep.provider] codex_app_server requires model and reasoning_effort")
	}
	return c.validateCommonEnabledPolicy()
}

func (c Config) validateCommonEnabledPolicy() error {
	if err := validatePosture("retention", c.Provider.RetentionPosture); err != nil {
		return err
	}
	if err := validatePosture("training", c.Provider.TrainingPosture); err != nil {
		return err
	}
	if err := validateSources(c.Provider.AllowedSources); err != nil {
		return err
	}
	if err := validateDate("source_since", c.Provider.SourceSince, false); err != nil {
		return err
	}
	if err := validateDate("source_until", c.Provider.SourceUntil, true); err != nil {
		return err
	}
	if c.Provider.SourceUntil != "" && c.Provider.SourceUntil < c.Provider.SourceSince {
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
	endpoint := (*url.URL)(nil)
	if c.Provider.Kind == ProviderOpenAICompatible {
		var err error
		endpoint, _, err = validateEndpoint(c.Provider.Endpoint)
		if err != nil {
			return ProviderProfile{}, err
		}
	}
	sources := slices.Clone(c.Provider.AllowedSources)
	slices.Sort(sources)
	policy := providerPolicy{
		Kind:  c.Provider.Kind,
		Model: strings.TrimSpace(c.Provider.Model), APIKeyEnv: c.Provider.APIKeyEnv,
		AllowAnonymous:   c.Provider.AllowAnonymous,
		RetentionPosture: strings.TrimSpace(c.Provider.RetentionPosture),
		TrainingPosture:  strings.TrimSpace(c.Provider.TrainingPosture),
		AllowedSources:   sources, SourceSince: c.Provider.SourceSince,
		SourceUntil: c.Provider.SourceUntil, AllowSensitive: c.Provider.AllowSensitive,
		ReasoningEffort:       strings.TrimSpace(c.Provider.ReasoningEffort),
		ExecutionBoundary:     c.Provider.ExecutionBoundary,
		PacketRendererPolicy:  PacketRendererPolicyV1,
		ProgramFingerprint:    ProgramFingerprint(),
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
		Fingerprint: hex.EncodeToString(digest[:]),
		Kind:        policy.Kind, Endpoint: policy.Endpoint, Model: policy.Model,
		APIKeyEnv: policy.APIKeyEnv, AllowAnonymous: policy.AllowAnonymous,
		RetentionPosture: policy.RetentionPosture, TrainingPosture: policy.TrainingPosture,
		AllowedSources: slices.Clone(policy.AllowedSources), SourceSince: policy.SourceSince,
		SourceUntil: policy.SourceUntil, AllowSensitive: policy.AllowSensitive,
		ReasoningEffort: policy.ReasoningEffort, ExecutionBoundary: policy.ExecutionBoundary,
		PacketRendererPolicy:  policy.PacketRendererPolicy,
		ProgramFingerprint:    policy.ProgramFingerprint,
		DisclosedPacketFields: slices.Clone(policy.DisclosedPacketFields),
		PolicyJSON:            policyJSON,
	}, nil
}

// Validate proves the profile fields, canonical policy bytes, and fingerprint
// still describe exactly the same policy.
func (p ProviderProfile) Validate() error {
	config := Config{Enabled: true, Provider: ProviderConfig{
		Kind: p.Kind, Endpoint: p.Endpoint, Model: p.Model, APIKeyEnv: p.APIKeyEnv,
		AllowAnonymous: p.AllowAnonymous, RetentionPosture: p.RetentionPosture,
		TrainingPosture: p.TrainingPosture, AllowedSources: slices.Clone(p.AllowedSources),
		SourceSince: p.SourceSince, SourceUntil: p.SourceUntil,
		AllowSensitive: p.AllowSensitive, ReasoningEffort: p.ReasoningEffort,
		ExecutionBoundary: p.ExecutionBoundary, RequestTimeout: time.Second,
	}}
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
	if p.Kind != want.Kind || p.Endpoint != want.Endpoint || p.Model != want.Model ||
		p.APIKeyEnv != want.APIKeyEnv || p.AllowAnonymous != want.AllowAnonymous ||
		p.RetentionPosture != want.RetentionPosture ||
		p.TrainingPosture != want.TrainingPosture ||
		!slices.Equal(p.AllowedSources, want.AllowedSources) ||
		p.SourceSince != want.SourceSince || p.SourceUntil != want.SourceUntil ||
		p.AllowSensitive != want.AllowSensitive || p.ReasoningEffort != want.ReasoningEffort ||
		p.ExecutionBoundary != want.ExecutionBoundary ||
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
