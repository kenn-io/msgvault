package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const webhookConfig = `[meta_webhooks]
listen_addr="127.0.0.1:8791"
[[meta_webhooks.apps]]
app_id="9"
app_secret_env="EXAMPLE_META_APP_SECRET"
verify_token_env="EXAMPLE_META_VERIFY_TOKEN"
`

func TestMetaWebhooksDefaults(t *testing.T) {
	cfg := loadConfigText(t, webhookConfig)
	cfg.Data.DataDir = t.TempDir()
	assert.True(t, cfg.MetaWebhooks.Enabled())
	body, spool, retention := cfg.MetaWebhooks.Effective()
	assert.Equal(t, int64(8388608), body)
	assert.Equal(t, int64(1073741824), spool)
	assert.Equal(t, 720*time.Hour, retention)
	assert.False(t, NewDefaultConfig().MetaWebhooks.Enabled())
	body, spool, retention = (MetaWebhooksConfig{}).Effective()
	assert.Equal(t, int64(8388608), body)
	assert.Equal(t, int64(1073741824), spool)
	assert.Equal(t, 720*time.Hour, retention)
}
func TestMetaWebhooksValidation(t *testing.T) {
	for _, tt := range []struct{ name, text, field string }{
		{"unknown section key", strings.Replace(webhookConfig, "[meta_webhooks]", "[meta_webhooks]\nunknown=true", 1), "unknown"},
		{"unknown app key", webhookConfig + "app_secret=\"not-allowed\"\n", "unknown"},
		{"nondecimal app", strings.Replace(webhookConfig, `app_id="9"`, `app_id="9_1"`, 1), "app_id"},
		{"duplicate app", webhookConfig + "[[meta_webhooks.apps]]" + strings.Split(webhookConfig, "[[meta_webhooks.apps]]")[1], "duplicate"},
		{"bad secret env", strings.Replace(webhookConfig, "EXAMPLE_META_APP_SECRET", "TOKEN=value", 1), "app_secret_env"},
		{"bad verify env", strings.Replace(webhookConfig, "EXAMPLE_META_VERIFY_TOKEN", "", 1), "verify_token_env"},
		{"bad address", strings.Replace(webhookConfig, "127.0.0.1:8791", "missing-port", 1), "listen_addr"},
		{"zero port", strings.Replace(webhookConfig, "127.0.0.1:8791", "127.0.0.1:0", 1), "listen_addr"},
		{"large port", strings.Replace(webhookConfig, "127.0.0.1:8791", "127.0.0.1:65536", 1), "listen_addr"},
		{"server conflict", "[server]\nbind_addr=\"127.0.0.1\"\napi_port=8791\n" + webhookConfig, "server"},
		{"zero body cap", strings.Replace(webhookConfig, "[meta_webhooks]", "[meta_webhooks]\nmax_body_bytes=0", 1), "max_body_bytes"},
		{"negative spool cap", strings.Replace(webhookConfig, "[meta_webhooks]", "[meta_webhooks]\nmax_spool_bytes=-1", 1), "max_spool_bytes"},
		{"no address", strings.Replace(webhookConfig, `listen_addr="127.0.0.1:8791"`, "", 1), "listen_addr"},
		{"no apps", "[meta_webhooks]\nlisten_addr=\"127.0.0.1:8791\"\n", "apps"},
		{"bad retention", strings.Replace(webhookConfig, "[meta_webhooks]", "[meta_webhooks]\ndone_retention=\"tomorrow\"", 1), "done_retention"},
		{"zero retention", strings.Replace(webhookConfig, "[meta_webhooks]", "[meta_webhooks]\ndone_retention=\"0h\"", 1), "done_retention"},
	} {
		t.Run(tt.name, func(t *testing.T) { require.ErrorContains(t, loadConfigTextError(t, tt.text), tt.field) })
	}
}
func TestMetaWebhooksSecretsNeverSerialised(t *testing.T) {
	t.Setenv("EXAMPLE_META_APP_SECRET", "synthetic-secret-value")
	t.Setenv("EXAMPLE_META_VERIFY_TOKEN", "synthetic-verify-value")
	cfg := loadConfigText(t, webhookConfig)
	cfg.Data.DataDir = t.TempDir()
	require.NoError(t, cfg.Save())
	data, err := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(t, err)
	assert.NotContains(t, string(data), "synthetic-secret-value")
	assert.NotContains(t, string(data), "synthetic-verify-value")
	loaded, err := Load(cfg.ConfigFilePath(), cfg.HomeDir)
	require.NoError(t, err)
	assert.Equal(t, cfg.MetaWebhooks, loaded.MetaWebhooks)
}
