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
	require.NoError(os.WriteFile(path, []byte("[integrations.docbank]\nreference_consent = true\nreference_origins = [\"https://cap.example.test\"]\n"), 0600))
	cfg, err := Load(path, "")
	require.NoError(err)
	assert.True(cfg.Integrations.Docbank.ReferenceConsent)
	assert.Equal([]string{"https://cap.example.test"}, cfg.Integrations.Docbank.ReferenceOrigins)
	require.NoError(os.WriteFile(path, []byte("[integrations.docbank]\nreference_origins = [\"https://cap.example.test\", \"https://example.test/s/private-token\"]\n"), 0600))
	_, err = Load(path, "")
	require.Error(err)
	assert.Contains(err.Error(), "reference_origins[2]: invalid origin")
	assert.NotContains(err.Error(), "private-token")
}
