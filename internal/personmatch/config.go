// Package personmatch defines the disabled-by-default Jev scoring boundary.
package personmatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
)

const (
	ModelID         = "jev-1.13.0"
	Endpoint        = "https://api.typesafe.ai/v1/systemone"
	PacketSchema    = "person-match-packet-v1"
	PolicyVersion   = "person-match-policy-v1"
	QuestionVersion = "same_person_v1"
	defaultBatch    = 20
	maxBatch        = 100
)

var credentialEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Config contains only provider settings and a credential reference. It never
// contains the credential value. RetentionDeclaration is the operator's exact
// declaration of the provider policy accepted for this disclosure.
type Config struct {
	Enabled              bool    `toml:"enabled"`
	ModelID              string  `toml:"model_id"`
	MinimumProbability   float64 `toml:"minimum_probability"`
	CredentialEnv        string  `toml:"credential_env"`
	BatchSize            int     `toml:"batch_size"`
	RetentionDeclaration string  `toml:"retention_declaration"`
}

func (c *Config) ApplyDefaults() {
	if c.ModelID == "" {
		c.ModelID = ModelID
	}
	if c.MinimumProbability == 0 {
		c.MinimumProbability = 0.80
	}
	if c.BatchSize == 0 {
		c.BatchSize = defaultBatch
	}
}

func (c *Config) Validate() error {
	if c.ModelID != ModelID {
		return fmt.Errorf("people.identity_merge.model_id must be %s", ModelID)
	}
	if math.IsNaN(c.MinimumProbability) || c.MinimumProbability < 0.80 || c.MinimumProbability > 1.0 {
		return errors.New("people.identity_merge.minimum_probability must be between 0.80 and 1.00")
	}
	if c.BatchSize < 1 || c.BatchSize > maxBatch {
		return fmt.Errorf("people.identity_merge.batch_size must be between 1 and %d", maxBatch)
	}
	if c.CredentialEnv != "" && !credentialEnvName.MatchString(c.CredentialEnv) {
		return errors.New("people.identity_merge.credential_env must be an environment variable name")
	}
	if !c.Enabled {
		return nil
	}
	if c.CredentialEnv == "" {
		return errors.New("people.identity_merge.credential_env is required when enabled")
	}
	if strings.TrimSpace(c.RetentionDeclaration) == "" || c.RetentionDeclaration != strings.TrimSpace(c.RetentionDeclaration) {
		return errors.New("people.identity_merge.retention_declaration is required when enabled")
	}
	return nil
}

// Disclosure contains all policy inputs that must remain unchanged for a
// recorded consent to authorize the next scoring request.
type Disclosure struct {
	Endpoint             string `json:"endpoint"`
	ModelID              string `json:"model_id"`
	PacketSchema         string `json:"packet_schema"`
	RetentionDeclaration string `json:"retention_declaration"`
	PolicyVersion        string `json:"policy_version"`
	QuestionVersion      string `json:"question_version"`
}

func (d Disclosure) Fingerprint() (string, error) {
	for _, value := range []string{d.Endpoint, d.ModelID, d.PacketSchema, d.RetentionDeclaration, d.PolicyVersion, d.QuestionVersion} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return "", errors.New("person match disclosure fields must be nonempty and canonical")
		}
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("encode person match disclosure: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Config) Disclosure() (Disclosure, error) {
	if err := c.Validate(); err != nil {
		return Disclosure{}, err
	}
	if !c.Enabled {
		return Disclosure{}, errors.New("person match scoring is disabled")
	}
	return Disclosure{Endpoint: Endpoint, ModelID: c.ModelID, PacketSchema: PacketSchema,
		RetentionDeclaration: c.RetentionDeclaration, PolicyVersion: PolicyVersion,
		QuestionVersion: QuestionVersion}, nil
}

func (c *Config) DisclosureFingerprint() (string, error) {
	disclosure, err := c.Disclosure()
	if err != nil {
		return "", err
	}
	return disclosure.Fingerprint()
}

// CredentialFromEnvironment is for the daemon scorer only. The caller must
// keep the returned value out of logs and persisted records.
func (c *Config) CredentialFromEnvironment() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	if !c.Enabled {
		return "", errors.New("person match scoring is disabled")
	}
	value, exists := os.LookupEnv(c.CredentialEnv)
	if !exists || strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
		return "", errors.New("person match credential is missing or invalid")
	}
	return value, nil
}
