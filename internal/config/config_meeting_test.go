package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeMeetingConfig(t *testing.T, content string) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", tmpDir)
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0644), "WriteFile()")
	return configPath
}

func TestLoadWithMeetingSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	configPath := writeMeetingConfig(t, `
[[granola]]
identifier = "alice@example.com"
account_email = "alice@example.com"
api_key = "grn_test1"
schedule = "0 */6 * * *"
enabled = true

[[granola]]
identifier = "work"
account_email = "work@example.com"
api_key = "grn_test2"
enabled = true

[[circleback]]
identifier = "alice@example.com"
account_email = "alice@example.com"
schedule = "30 */6 * * *"
enabled = true
`)

	cfg, err := Load(configPath, "")
	require.NoError(err, "Load()")

	require.Len(cfg.Granola, 2)
	assert.Equal("grn_test1", cfg.Granola[0].APIKey)

	src := cfg.GetGranolaSource("ALICE@example.com")
	require.NotNil(src, "lookup is case-insensitive")
	assert.Equal("grn_test1", src.APIKey)
	assert.Nil(cfg.GetGranolaSource("nope"))

	scheduled := cfg.ScheduledGranolaSources()
	require.Len(scheduled, 1, "only entries with schedule + enabled")
	assert.Equal("alice@example.com", scheduled[0].Identifier)

	cb := cfg.GetCirclebackSource("alice@example.com")
	require.NotNil(cb)
	require.Len(cfg.ScheduledCirclebackSources(), 1)
}

func TestLoadMeetingSourceSingleEntryDefaultsIdentifier(t *testing.T) {
	require := require.New(t)
	configPath := writeMeetingConfig(t, `
[[granola]]
account_email = "granola@example.com"
api_key = "grn_test"
enabled = true

[[circleback]]
account_email = "circleback@example.com"
enabled = true
`)

	cfg, err := Load(configPath, "")
	require.NoError(err, "Load()")
	require.Equal("default", cfg.Granola[0].Identifier)
	require.Equal("default", cfg.Circleback[0].Identifier)
}

func TestMeetingSourceEffectiveAccountEmail(t *testing.T) {
	tests := []struct {
		name         string
		provider     string
		config       string
		wantEmail    string
		wantErrParts []string
	}{
		{
			name:      "granola explicit account email is normalized and preferred",
			provider:  "granola",
			config:    "identifier = \"label@example.net\"\naccount_email = \"  User-A@Example.COM  \"\napi_key = \"grn_test\"",
			wantEmail: "user-a@example.com",
		},
		{
			name:      "circleback explicit account email is normalized and preferred",
			provider:  "circleback",
			config:    "identifier = \"label@example.net\"\naccount_email = \"  User-A@Example.COM  \"",
			wantEmail: "user-a@example.com",
		},
		{
			name:         "granola email identifier still requires account email",
			provider:     "granola",
			config:       "identifier = \"  User-A@Example.COM  \"\napi_key = \"grn_test\"",
			wantErrParts: []string{"identifier \"User-A@Example.COM\"", "requires", "account_email"},
		},
		{
			name:         "circleback email identifier still requires account email",
			provider:     "circleback",
			config:       "identifier = \"  User-A@Example.COM  \"",
			wantErrParts: []string{"identifier \"User-A@Example.COM\"", "requires", "account_email"},
		},
		{
			name:         "granola label requires explicit account email",
			provider:     "granola",
			config:       "identifier = \"work\"\napi_key = \"grn_test\"",
			wantErrParts: []string{"identifier \"work\"", "preserve", "account_email"},
		},
		{
			name:         "circleback label requires explicit account email",
			provider:     "circleback",
			config:       "identifier = \"default\"",
			wantErrParts: []string{"identifier \"default\"", "preserve", "account_email"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			configPath := writeMeetingConfig(t, "[["+tt.provider+"]]\n"+tt.config+"\n")
			cfg, err := Load(configPath, "")
			if len(tt.wantErrParts) > 0 {
				require.Error(err)
				for _, part := range tt.wantErrParts {
					assert.Contains(err.Error(), part)
				}
				return
			}
			require.NoError(err)
			if tt.provider == "granola" {
				require.Len(cfg.Granola, 1)
				got, effectiveErr := cfg.Granola[0].EffectiveAccountEmail()
				require.NoError(effectiveErr)
				assert.Equal(tt.wantEmail, got)
				return
			}
			require.Len(cfg.Circleback, 1)
			got, effectiveErr := cfg.Circleback[0].EffectiveAccountEmail()
			require.NoError(effectiveErr)
			assert.Equal(tt.wantEmail, got)
		})
	}
}

func TestLoadMeetingSourceDuplicateIdentifiersRejected(t *testing.T) {
	require := require.New(t)
	configPath := writeMeetingConfig(t, `
[[granola]]
identifier = "same"
api_key = "grn_a"

[[granola]]
identifier = "SAME"
api_key = "grn_b"
`)

	_, err := Load(configPath, "")
	require.Error(err)
	require.Contains(err.Error(), "duplicate identifier")
}

func TestLoadMeetingSourceMissingIdentifierRejected(t *testing.T) {
	require := require.New(t)
	configPath := writeMeetingConfig(t, `
[[circleback]]
enabled = true

[[circleback]]
identifier = "second"
`)

	_, err := Load(configPath, "")
	require.Error(err)
	require.Contains(err.Error(), "identifier")
}
