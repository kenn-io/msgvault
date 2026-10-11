package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBeeperDocbankConfig(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	t.Setenv("MSGVAULT_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[integrations.docbank]
enabled = true
url = "http://127.0.0.1:8080"
api_key_env = "DOCBANK_TEST_KEY"
all_sources_upload_consent = true
asr_profile = "voice-asr"
reference_consent = true
reference_origins = ["https://cap.example.test"]
`
	require.NoError(os.WriteFile(path, []byte(content), 0o600))

	cfg, err := Load(path, "")
	require.NoError(err)
	assert.Equal(DocbankIntegrationConfig{
		Enabled: true, URL: "http://127.0.0.1:8080",
		APIKeyEnv: "DOCBANK_TEST_KEY", AllSourcesUploadConsent: true, ASRProfile: "voice-asr",
		ReferenceConsent: true, ReferenceOrigins: []string{"https://cap.example.test"},
	}, cfg.Integrations.Docbank)
	require.NoError(os.WriteFile(path, []byte("[integrations.docbank]\nreference_origins = [\"https://cap.example.test\", \"https://example.test/s/private-token\"]\n"), 0o600))
	_, err = Load(path, "")
	require.Error(err)
	assert.Contains(err.Error(), "reference_origins[2]: invalid origin")
	assert.NotContains(err.Error(), "private-token")
}

func TestBeeperDocbankConfigRejectsReservedSuppliedTranscriptProfile(t *testing.T) {
	t.Setenv("MSGVAULT_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[integrations.docbank]
enabled = true
url = "http://127.0.0.1:8080"
api_key_env = "DOCBANK_TEST_KEY"
all_sources_upload_consent = true
asr_profile = " supplied-transcript "
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	_, err := Load(path, "")
	require.ErrorContains(t, err, "reserved for supplied transcript input")
}

func TestBeeperDocbankConfigDefaultsDisabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("MSGVAULT_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(""), 0o600))

	cfg, err := Load(path, "")
	require.NoError(err)
	assert.False(cfg.Integrations.Docbank.Enabled)
	assert.False(cfg.Integrations.Docbank.AllSourcesUploadConsent)
}
