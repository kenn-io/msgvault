package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const messagesProfile = `identifier="example-inbox"
app_id="9"
page_id="100"
token_env="EXAMPLE_PAGE_TOKEN"
enabled=true
schedule="17 */6 * * *"
actions=["send", "human_agent"]
`

func checkMessagesConfig(t *testing.T, kind string, ig bool) {
	t.Helper()
	profile := "[[" + kind + "]]\n" + messagesProfile
	if ig {
		profile += "instagram_account_id=\"1784\"\n"
	}
	require.NoError(t, loadConfigTextError(t, profile))
	cases := [][2]string{
		{`identifier="example-inbox"`, `identifier=""`},
		{`app_id="9"`, `app_id="9/path"`},
		{`page_id="100"`, `page_id="100_200"`},
		{`token_env="EXAMPLE_PAGE_TOKEN"`, `token_env="TOKEN=value"`},
		{`schedule="17 */6 * * *"`, `schedule="tomorrow"`},
		{`actions=["send", "human_agent"]`, `actions=["human_agent"]`},
		{`actions=["send", "human_agent"]`, `actions=["unknown"]`},
		{`actions=["send", "human_agent"]`, `actions=["send", "send"]`},
	}
	if ig {
		cases = append(cases, [2]string{`instagram_account_id="1784"`, `instagram_account_id="bad"`})
	}
	for _, tc := range cases {
		t.Run(tc[1], func(t *testing.T) {
			require := require.New(t)

			require.Error(loadConfigTextError(t, strings.Replace(profile, tc[0], tc[1], 1)))
		})
	}
	require := require.New(t)
	require.Error(loadConfigTextError(t, profile+"token=\"inline-not-allowed\"\n"))
	other := strings.Replace(profile, `identifier="example-inbox"`, `identifier="another"`, 1)
	require.Error(loadConfigTextError(t, profile+other))
	other = strings.Replace(other, `page_id="100"`, `page_id="101"`, 1)
	if ig {
		other = strings.Replace(other, `instagram_account_id="1784"`, `instagram_account_id="1785"`, 1)
	}
	require.NoError(loadConfigTextError(t, profile+other))
	other = strings.Replace(other, `identifier="another"`, `identifier="EXAMPLE-INBOX"`, 1)
	require.Error(loadConfigTextError(t, profile+other))
}

func TestFacebookMessagesConfigValidation(t *testing.T) {
	checkMessagesConfig(t, "facebook_messages", false)
}
func TestInstagramMessagesConfigValidation(t *testing.T) {
	checkMessagesConfig(t, "instagram_messages", true)
}

func TestInstagramMessagesConfigWebhookRoute(t *testing.T) {
	for _, tc := range []struct {
		name          string
		firstEnabled  bool
		secondEnabled bool
		secondApp     string
		secondAccount string
		wantDuplicate bool
	}{
		{"duplicate enabled route", true, true, "9", "1784", true},
		{"disabled second profile", true, false, "9", "1784", false},
		{"disabled first profile", false, true, "9", "1784", false},
		{"separate app", true, true, "10", "1784", false},
		{"separate account", true, true, "9", "1785", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)

			profile := MetaMessagesSource{Identifier: "first", AppID: "9", PageID: "100", InstagramAccountID: "1784", TokenEnv: "EXAMPLE_PAGE_TOKEN", Enabled: tc.firstEnabled}
			other := profile
			other.Identifier, other.PageID = "second", "101"
			other.Enabled, other.AppID, other.InstagramAccountID = tc.secondEnabled, tc.secondApp, tc.secondAccount
			err := validateMetaMessagesSources("instagram_messages", []MetaMessagesSource{profile, other}, true)
			if tc.wantDuplicate {
				require.ErrorContains(err, "duplicates an enabled app/instagram_account_id webhook route")
			} else {
				require.NoError(err)
			}
		})
	}
}

func TestScheduledMessagesSources(t *testing.T) {
	assert := assert.New(t)

	cfg := loadConfigText(t, "[[facebook_messages]]\n"+messagesProfile+"[[instagram_messages]]\n"+messagesProfile+"instagram_account_id=\"1784\"\n")
	assert.Len(cfg.ScheduledFacebookMessagesSources(), 1)
	assert.Len(cfg.ScheduledInstagramMessagesSources(), 1)
	assert.NotNil(cfg.GetFacebookMessagesSource("EXAMPLE-INBOX"))
	assert.NotNil(cfg.GetInstagramMessagesSource("EXAMPLE-INBOX"))
	assert.Nil(cfg.GetFacebookMessagesSource("missing"))
	cfg.FacebookMessages[0].Enabled = false
	cfg.InstagramMessages[0].Schedule = ""
	assert.Empty(cfg.ScheduledFacebookMessagesSources())
	assert.Empty(cfg.ScheduledInstagramMessagesSources())
}
