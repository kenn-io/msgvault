// Package mailkind defines the policy for optional transactional-mail decisions.
package mailkind

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"math"
	"os"
	"strings"

	"go.kenn.io/msgvault/internal/envutil"
)

const (
	ProviderJev     = "jev"
	Endpoint        = "https://api.typesafe.ai/v1/systemone"
	ModelID         = "jev-1.13.0"
	PacketSchema    = "mail-kind-packet-v1"
	PolicyVersion   = "mail-kind-policy-v1"
	QuestionVersion = "mail-kind-questions-v1"
	DataFields      = "sender domain; subject (256 characters); authored text excerpt (1500 characters), with long digit runs and email local parts masked; attachment counts by MIME type; deterministic record types. No recipients, attachment bytes, or raw HTML."
)

// Config is disabled by default. Credentials are read only by the daemon.
type Config struct {
	Enabled                  bool    `toml:"enabled"`
	Provider                 string  `toml:"provider"`
	CredentialEnv            string  `toml:"credential_env"`
	RetentionDeclaration     string  `toml:"retention_declaration"`
	BatchSize                int     `toml:"batch_size"`
	MinimumTransactional     float64 `toml:"minimum_transactional"`
	MinimumChoiceProbability float64 `toml:"minimum_choice_probability"`
}

func (c *Config) ApplyDefaults() {
	if c.Provider == "" {
		c.Provider = ProviderJev
	}
	if c.CredentialEnv == "" {
		c.CredentialEnv = "TYPESAFE_API_KEY"
	}
	if c.BatchSize == 0 {
		c.BatchSize = 20
	}
	if c.MinimumTransactional == 0 {
		c.MinimumTransactional = 0.9
	}
	if c.MinimumChoiceProbability == 0 {
		c.MinimumChoiceProbability = 0.8
	}
}

func (c *Config) Validate() error {
	if c.Provider != ProviderJev {
		return errors.New("mail.kind_classification.provider must be jev")
	}
	if c.BatchSize < 1 || c.BatchSize > 100 {
		return errors.New("mail.kind_classification.batch_size must be between 1 and 100")
	}
	if !validProbability(c.MinimumTransactional, 0.9) || !validProbability(c.MinimumChoiceProbability, 0.8) {
		return errors.New("mail kind probability thresholds must be finite, at least 0.9 transactional and 0.8 choice, and at most 1")
	}
	if !envutil.ValidName(c.CredentialEnv) {
		return errors.New("mail.kind_classification.credential_env must be an environment variable name")
	}
	if c.Enabled && (c.RetentionDeclaration == "" || strings.TrimSpace(c.RetentionDeclaration) != c.RetentionDeclaration) {
		return errors.New("mail.kind_classification.retention_declaration is required and must be canonical when enabled")
	}
	return nil
}

func validProbability(value, floor float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= floor && value <= 1
}

// Disclosure is the complete immutable policy authorized by one consent grant.
// It contains the credential reference, never its value.
type Disclosure struct {
	Provider                 string  `json:"provider"`
	Endpoint                 string  `json:"endpoint"`
	ModelID                  string  `json:"model_id"`
	CredentialEnv            string  `json:"credential_env"`
	PacketSchema             string  `json:"packet_schema"`
	DataFields               string  `json:"data_fields"`
	RetentionDeclaration     string  `json:"retention_declaration"`
	PolicyVersion            string  `json:"policy_version"`
	QuestionVersion          string  `json:"question_version"`
	MinimumTransactional     float64 `json:"minimum_transactional"`
	MinimumChoiceProbability float64 `json:"minimum_choice_probability"`
}

func (c *Config) Disclosure() (Disclosure, error) {
	if err := c.Validate(); err != nil {
		return Disclosure{}, err
	}
	if !c.Enabled {
		return Disclosure{}, errors.New("mail kind classification is disabled")
	}
	return Disclosure{
		Provider: c.Provider, Endpoint: Endpoint, ModelID: ModelID,
		CredentialEnv: c.CredentialEnv, PacketSchema: PacketSchema, DataFields: DataFields,
		RetentionDeclaration: c.RetentionDeclaration, PolicyVersion: PolicyVersion,
		QuestionVersion: QuestionVersion, MinimumTransactional: c.MinimumTransactional,
		MinimumChoiceProbability: c.MinimumChoiceProbability,
	}, nil
}

func (d Disclosure) Fingerprint() (string, error) {
	// Only policies this binary can enforce may be granted.
	cfg := Config{Enabled: true, Provider: d.Provider, CredentialEnv: d.CredentialEnv,
		RetentionDeclaration: d.RetentionDeclaration, BatchSize: 20,
		MinimumTransactional: d.MinimumTransactional, MinimumChoiceProbability: d.MinimumChoiceProbability}
	canonical, err := cfg.Disclosure()
	if err != nil {
		return "", err
	}
	if canonical != d {
		return "", errors.New("mail kind disclosure does not match the supported policy")
	}
	encoded, err := json.Marshal(d, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Config) CredentialFromEnvironment() (string, error) {
	if _, err := c.Disclosure(); err != nil {
		return "", err
	}
	value := os.Getenv(c.CredentialEnv)
	if value == "" || value != strings.TrimSpace(value) {
		return "", errors.New("mail kind credential is missing or invalid")
	}
	return value, nil
}
