package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
)

func TestEditPersonEnrichmentProviderPreservesTargetExtensionsAndUnrelatedProviders(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := `[people.enrichment]
enabled = false

[[people.enrichment.providers]] # target table comment
name = "exa-primary" # stable identity
kind = "exa"
enabled = false # old value
endpoint = "https://old.example.test/search"
api_key_env = "HOST_OWNED_EXA_KEY" # must remain host-owned
future_target_option = "keep-target" # unknown target field

# unrelated provider comment
[[people.enrichment.providers]]
name = "exa-secondary"
kind = "exa"
enabled = false
endpoint = "https://secondary.example.test/search"
api_key_env = "SECONDARY_EXA_KEY"
future_unrelated_option = { nested = true } # unknown unrelated field
`
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	unrelated := `# unrelated provider comment
[[people.enrichment.providers]]
name = "exa-secondary"
kind = "exa"
enabled = false
endpoint = "https://secondary.example.test/search"
api_key_env = "SECONDARY_EXA_KEY"
future_unrelated_option = { nested = true } # unknown unrelated field
`
	updated := personenrichment.ProviderConfig{
		Name:               "exa-primary",
		Kind:               personenrichment.ProviderExa,
		Enabled:            false,
		Endpoint:           "https://new.example.test/search",
		Mode:               "people",
		NumResults:         1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierName},
		TargetKeys:         []string{"attribute:bio"},
		RetentionPosture:   "zero_retention",
		TrainingPosture:    "no_training",
		RefreshInterval:    24 * time.Hour,
		RequestTimeout:     time.Minute,
		PollInterval:       30 * time.Second,
		MaxJobAge:          15 * time.Minute,
		MaxRetries:         5,
		MaxRequestsPerRun:  20,
		MaxRequestsPerDay:  100,
	}

	written, err := EditPersonEnrichmentProvider(path, snapshot.ETag, "exa-primary", updated)
	requirements.NoError(err)
	content := string(written.Content)
	assertions.Contains(content, `[[people.enrichment.providers]] # target table comment`)
	assertions.Contains(content, `name = "exa-primary" # stable identity`)
	assertions.Contains(content, `endpoint = "https://new.example.test/search"`)
	assertions.Contains(content, `api_key_env = "HOST_OWNED_EXA_KEY" # must remain host-owned`)
	assertions.Contains(content, `future_target_option = "keep-target" # unknown target field`)
	assertions.Contains(content, unrelated)
}

func TestEditPersonEnrichmentProviderRejectsAmbiguousOrMissingStoredNames(t *testing.T) {
	tests := map[string]string{
		"duplicate": `[[people.enrichment.providers]]
name = "exa-primary"
[[people.enrichment.providers]]
name = "exa-primary"
`,
		"missing": `[[people.enrichment.providers]]
kind = "exa"
`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			snapshot, err := ReadConfigFile(path)
			require.NoError(t, err)

			_, err = EditPersonEnrichmentProvider(path, snapshot.ETag, "exa-primary", personenrichment.ProviderConfig{
				Name: "exa-primary", Kind: personenrichment.ProviderExa,
			})
			assert.ErrorIs(t, err, ErrAmbiguousConfigTarget)
		})
	}
}
