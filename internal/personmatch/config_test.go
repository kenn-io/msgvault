package personmatch

import (
	"encoding/json/v2"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonMatchConfigDefaultsAndValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := Config{}
	cfg.ApplyDefaults()
	assert.False(cfg.Enabled)
	assert.Equal("jev-1.13.0", cfg.ModelID)
	assert.InDelta(0.80, cfg.MinimumProbability, 1e-9)
	assert.Positive(cfg.BatchSize)
	assert.LessOrEqual(cfg.BatchSize, 100)
	require.NoError(cfg.Validate())

	cfg.Enabled = true
	cfg.CredentialEnv = "JEV_SCORER_KEY"
	cfg.RetentionDeclaration = "operator-confirmed-retention-v1"
	require.NoError(cfg.Validate())

	for name, mutate := range map[string]func(*Config){
		"moving model":    func(c *Config) { c.ModelID = "jev-latest" },
		"low threshold":   func(c *Config) { c.MinimumProbability = 0.79 },
		"high threshold":  func(c *Config) { c.MinimumProbability = 1.01 },
		"bad variable":    func(c *Config) { c.CredentialEnv = "JEV-KEY" },
		"empty variable":  func(c *Config) { c.CredentialEnv = "" },
		"empty retention": func(c *Config) { c.RetentionDeclaration = "" },
		"large batch":     func(c *Config) { c.BatchSize = 101 },
		"not a number":    func(c *Config) { c.MinimumProbability = math.NaN() },
		"infinite":        func(c *Config) { c.MinimumProbability = math.Inf(1) },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := cfg
			mutate(&invalid)
			assert.Error(invalid.Validate())
		})
	}
}

func TestPersonMatchConfigDisclosureFingerprintChangesWithEachBoundary(t *testing.T) {
	base := Disclosure{
		Endpoint: "https://api.typesafe.ai/v1/systemone", ModelID: "jev-1.13.0",
		PacketSchema: "person-match-packet-v1", RetentionDeclaration: "operator-confirmed-retention-v1",
		PolicyVersion: "person-match-policy-v1", QuestionVersion: QuestionVersion,
	}
	fingerprint, err := base.Fingerprint()
	require.NoError(t, err)
	assert.Len(t, fingerprint, 64)
	for name, mutate := range map[string]func(*Disclosure){
		"endpoint":  func(d *Disclosure) { d.Endpoint += "/changed" },
		"model":     func(d *Disclosure) { d.ModelID = "jev-1.13.1" },
		"packet":    func(d *Disclosure) { d.PacketSchema = "person-match-packet-v2" },
		"retention": func(d *Disclosure) { d.RetentionDeclaration = "operator-confirmed-retention-v2" },
		"policy":    func(d *Disclosure) { d.PolicyVersion = "person-match-policy-v2" },
		"question":  func(d *Disclosure) { d.QuestionVersion = "same_person_v2" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			got, err := changed.Fingerprint()
			require.NoError(t, err)
			assert.NotEqual(t, fingerprint, got)
		})
	}
	base.Endpoint = ""
	_, err = base.Fingerprint()
	assert.Error(t, err)
}

func TestPersonMatchDisclosureFingerprintIncludesProviderQuestionVersion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := Config{Enabled: true, ModelID: ModelID, MinimumProbability: 0.8,
		CredentialEnv: "JEV_SCORER_KEY", BatchSize: 10,
		RetentionDeclaration: "operator-confirmed-retention-v1"}
	disclosure, err := cfg.Disclosure()
	require.NoError(err)
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(err)

	encoded, err := json.Marshal(disclosure)
	require.NoError(err)
	var fields map[string]string
	require.NoError(json.Unmarshal(encoded, &fields))
	questionVersion, ok := fields["question_version"]
	require.True(ok, "provider question version must be part of the exact consent disclosure")
	assert.Equal("same_person_v1", questionVersion)

	changed := disclosure
	changed.QuestionVersion = "same_person_v2"
	changedFingerprint, err := changed.Fingerprint()
	require.NoError(err)
	assert.NotEqual(fingerprint, changedFingerprint,
		"changing provider question wording must require fresh consent")
}

func TestPersonMatchConfigMissingCredentialFailsClosed(t *testing.T) {
	cfg := Config{Enabled: true, ModelID: "jev-1.13.0", MinimumProbability: 0.8,
		CredentialEnv: "JEV_SCORER_KEY", BatchSize: 10,
		RetentionDeclaration: "operator-confirmed-retention-v1"}
	require.NoError(t, cfg.Validate())
	t.Setenv("JEV_SCORER_KEY", "")
	_, err := cfg.CredentialFromEnvironment()
	assert.Error(t, err)
}
