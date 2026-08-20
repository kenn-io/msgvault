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

	SourceConversationText  SourceClass = "conversation_text"
	SourceMeetingText       SourceClass = "meeting_text"
	SourceAttachmentCaption SourceClass = "attachment_caption"
	SourceAttachmentOCR     SourceClass = "attachment_ocr"
	SourceDocumentText      SourceClass = "document_text"
)

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
	Enabled  bool           `toml:"enabled"`
	Provider ProviderConfig `toml:"provider"`
}

// ProviderConfig contains both runtime settings and the exact outbound-data
// policy that must be consented before use.
type ProviderConfig struct {
	Kind             string        `toml:"kind"`
	Endpoint         string        `toml:"endpoint"`
	Model            string        `toml:"model"`
	APIKeyEnv        string        `toml:"api_key_env"`
	AllowAnonymous   bool          `toml:"allow_anonymous"`
	RetentionPosture string        `toml:"retention_posture"`
	TrainingPosture  string        `toml:"training_posture"`
	AllowedSources   []SourceClass `toml:"allowed_sources"`
	SourceSince      string        `toml:"source_since"`
	SourceUntil      string        `toml:"source_until"`
	AllowSensitive   bool          `toml:"allow_sensitive"`
	RequestTimeout   time.Duration `toml:"request_timeout"`
}

// ProviderProfile is one immutable, fingerprinted egress policy. PolicyJSON is
// canonical and intentionally excludes the credential value and request
// timeout.
type ProviderProfile struct {
	Fingerprint      string          `json:"fingerprint"`
	Kind             string          `json:"kind"`
	Endpoint         string          `json:"endpoint"`
	Model            string          `json:"model"`
	APIKeyEnv        string          `json:"api_key_env"`
	AllowAnonymous   bool            `json:"allow_anonymous"`
	RetentionPosture string          `json:"retention_posture"`
	TrainingPosture  string          `json:"training_posture"`
	AllowedSources   []SourceClass   `json:"allowed_sources"`
	SourceSince      string          `json:"source_since"`
	SourceUntil      string          `json:"source_until"`
	AllowSensitive   bool            `json:"allow_sensitive"`
	PolicyJSON       json.RawMessage `json:"-"`
}

type providerPolicy struct {
	Kind             string        `json:"kind"`
	Endpoint         string        `json:"endpoint"`
	Model            string        `json:"model"`
	APIKeyEnv        string        `json:"api_key_env"`
	AllowAnonymous   bool          `json:"allow_anonymous"`
	RetentionPosture string        `json:"retention_posture"`
	TrainingPosture  string        `json:"training_posture"`
	AllowedSources   []SourceClass `json:"allowed_sources"`
	SourceSince      string        `json:"source_since"`
	SourceUntil      string        `json:"source_until"`
	AllowSensitive   bool          `json:"allow_sensitive"`
}

// ApplyDefaults fills operational defaults without enabling inference.
func (c *Config) ApplyDefaults() {
	if c.Provider.Kind == "" {
		c.Provider.Kind = ProviderOpenAICompatible
	}
	if c.Provider.Endpoint == "" {
		c.Provider.Endpoint = "https://api.openai.com/v1"
	}
	if c.Provider.APIKeyEnv == "" && !c.Provider.AllowAnonymous {
		c.Provider.APIKeyEnv = "OPENAI_API_KEY"
	}
	if c.Provider.RequestTimeout == 0 {
		c.Provider.RequestTimeout = time.Minute
	}
}

// Validate rejects unsafe or ambiguous runtime configuration. An incomplete
// disabled policy is permitted, but any configured structural value must be
// well formed.
func (c Config) Validate() error {
	if c.Provider.Kind != ProviderOpenAICompatible {
		return fmt.Errorf("invalid [people.sweep.provider] kind %q", c.Provider.Kind)
	}
	if c.Provider.RequestTimeout <= 0 {
		return fmt.Errorf("invalid [people.sweep.provider] request_timeout %s: must be positive",
			c.Provider.RequestTimeout)
	}
	endpoint, loopback, err := validateEndpoint(c.Provider.Endpoint)
	if err != nil {
		return err
	}
	if c.Provider.APIKeyEnv != "" && !environmentNamePattern.MatchString(c.Provider.APIKeyEnv) {
		return fmt.Errorf("invalid [people.sweep.provider] api_key_env %q", c.Provider.APIKeyEnv)
	}
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Provider.Model) == "" {
		return errors.New("[people.sweep.provider] model is required when people sweep is enabled")
	}
	if c.Provider.AllowAnonymous {
		if c.Provider.APIKeyEnv != "" {
			return errors.New("[people.sweep.provider] anonymous mode cannot also configure api_key_env")
		}
		if !loopback {
			return errors.New("[people.sweep.provider] anonymous mode requires a loopback endpoint")
		}
	} else if c.Provider.APIKeyEnv == "" {
		return errors.New("[people.sweep.provider] api_key_env is required unless anonymous loopback mode is enabled")
	}
	if endpoint.Scheme == "http" && !loopback {
		return errors.New("[people.sweep.provider] remote endpoint must use HTTPS")
	}
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
	endpoint, _, err := validateEndpoint(c.Provider.Endpoint)
	if err != nil {
		return ProviderProfile{}, err
	}
	sources := slices.Clone(c.Provider.AllowedSources)
	slices.Sort(sources)
	policy := providerPolicy{
		Kind: c.Provider.Kind, Endpoint: canonicalEndpoint(endpoint),
		Model: strings.TrimSpace(c.Provider.Model), APIKeyEnv: c.Provider.APIKeyEnv,
		AllowAnonymous:   c.Provider.AllowAnonymous,
		RetentionPosture: strings.TrimSpace(c.Provider.RetentionPosture),
		TrainingPosture:  strings.TrimSpace(c.Provider.TrainingPosture),
		AllowedSources:   sources, SourceSince: c.Provider.SourceSince,
		SourceUntil: c.Provider.SourceUntil, AllowSensitive: c.Provider.AllowSensitive,
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
		PolicyJSON: policyJSON,
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
		AllowSensitive: p.AllowSensitive, RequestTimeout: time.Second,
	}}
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
		p.AllowSensitive != want.AllowSensitive {
		return errors.New("people inference provider profile fields are not canonical")
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
		case SourceConversationText, SourceMeetingText, SourceAttachmentCaption,
			SourceAttachmentOCR, SourceDocumentText:
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
