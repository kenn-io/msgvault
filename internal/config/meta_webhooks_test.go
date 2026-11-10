package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const metaWebhooksSection = `[meta_webhooks]
listen_addr="127.0.0.1:8787"
[[meta_webhooks.apps]]
app_id="9"
app_secret_env="EXAMPLE_META_APP_SECRET"
verify_token_env="EXAMPLE_META_VERIFY_TOKEN"
`

func TestMetaWebhooksConfig(t *testing.T) {
	cfg := loadConfigText(t, metaWebhooksSection)
	assert.Equal(t, []MetaWebhookApp{{AppID: "9", AppSecretEnv: "EXAMPLE_META_APP_SECRET", VerifyTokenEnv: "EXAMPLE_META_VERIFY_TOKEN"}}, cfg.MetaWebhooks.Apps)
}

func TestMetaWebhooksRejectsInvalidConfig(t *testing.T) {
	for name, text := range map[string]string{
		"missing port":       strings.Replace(metaWebhooksSection, `"127.0.0.1:8787"`, `"127.0.0.1"`, 1),
		"no apps":            "[meta_webhooks]\nlisten_addr=\"127.0.0.1:8787\"\n",
		"apps without addr":  strings.Replace(metaWebhooksSection, `listen_addr="127.0.0.1:8787"`, "", 1),
		"non-decimal app":    strings.Replace(metaWebhooksSection, `app_id="9"`, `app_id="9/x"`, 1),
		"duplicate app":      metaWebhooksSection + "[[meta_webhooks.apps]]\napp_id=\"9\"\napp_secret_env=\"A\"\nverify_token_env=\"B\"\n",
		"invalid secret env": strings.Replace(metaWebhooksSection, `app_secret_env="EXAMPLE_META_APP_SECRET"`, `app_secret_env="A=B"`, 1),
		"inline secret":      metaWebhooksSection + `app_secret="inline"` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, loadConfigTextError(t, text))
		})
	}
}
