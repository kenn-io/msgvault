package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocbankReferenceConfig(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[integrations.docbank]\nreference_consent = true\nreference_origins = [\"https://cap.example.test\", \"https://[2001:db8::1]:8443\"]\n"), 0600))
	cfg, err := Load(path, "")
	require.NoError(err)
	assert.True(cfg.Integrations.Docbank.ReferenceConsent)
	assert.Equal([]string{"https://cap.example.test", "https://[2001:db8::1]:8443"}, cfg.Integrations.Docbank.ReferenceOrigins)
	require.NoError(cfg.Save())
	reloaded, err := Load(path, "")
	require.NoError(err)
	assert.Equal(cfg.Integrations.Docbank, reloaded.Integrations.Docbank)
	require.NoError(os.WriteFile(path, []byte("[integrations.docbank]\nreference_origins = [\"https://example.test/s/token\"]\n"), 0600))
	_, err = Load(path, "")
	assert.Error(err)
}
