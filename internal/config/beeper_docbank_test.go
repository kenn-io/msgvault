package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBeeperDocbankConfig(t *testing.T) {
	t.Setenv("MSGVAULT_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[integrations.docbank]
enabled = true
url = "http://127.0.0.1:8080"
api_key_env = "DOCBANK_TEST_KEY"
upload_consent = true
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := Load(path, "")
	require.NoError(t, err)
	assert.Equal(t, DocbankIntegrationConfig{
		Enabled: true, URL: "http://127.0.0.1:8080",
		APIKeyEnv: "DOCBANK_TEST_KEY", UploadConsent: true,
	}, cfg.Integrations.Docbank)
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
	assert.False(cfg.Integrations.Docbank.UploadConsent)
}
