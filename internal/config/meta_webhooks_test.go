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

// relinkedInstagramProfiles is the same app and Instagram account under two
// Pages, as left behind when an account is relinked to another Page.
const relinkedInstagramProfiles = `[[instagram_comments]]
identifier="old-page"
app_id="9"
page_id="100"
instagram_account_id="200"
token_env="EXAMPLE_OLD_PAGE_TOKEN"
enabled=true

[[instagram_comments]]
identifier="new-page"
app_id="9"
page_id="101"
instagram_account_id="200"
token_env="EXAMPLE_NEW_PAGE_TOKEN"
enabled=true
`

func TestMetaWebhooksRejectsTwoInstagramProfilesForOneAccount(t *testing.T) {
	require := require.New(t)
	err := loadConfigTextError(t, metaWebhooksSection+relinkedInstagramProfiles)
	require.ErrorContains(err, "instagram_comments[1] and instagram_comments[0] both receive webhooks for app 9 and Instagram account 200")

	// A repeated Page is the profile section's own error, not this one.
	err = loadConfigTextError(t, metaWebhooksSection+strings.Replace(relinkedInstagramProfiles, `page_id="101"`, `page_id="100"`, 1))
	require.ErrorContains(err, "instagram_comments[1] duplicates an app/Page/Instagram tuple")
}

func TestMetaWebhooksAllowsInstagramProfilesThatCannotCollide(t *testing.T) {
	for name, text := range map[string]string{
		"no receiver":    relinkedInstagramProfiles,
		"one disabled":   metaWebhooksSection + strings.Replace(relinkedInstagramProfiles, "enabled=true", "enabled=false", 1),
		"app not listed": strings.Replace(metaWebhooksSection, `app_id="9"`, `app_id="8"`, 1) + relinkedInstagramProfiles,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Len(t, loadConfigText(t, text).InstagramComments, 2)
		})
	}
}
