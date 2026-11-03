package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const broadcastProfile = `[[messenger_broadcasts]]
identifier="example-offers"
app_id="9"
page_id="100"
ad_account_id="700"
token_env="EXAMPLE_PAGE_TOKEN"
ad_token_env="EXAMPLE_AD_TOKEN"
schedule="17 */6 * * *"
`

func broadcastConfigText(t *testing.T, profile string) string {
	t.Helper()
	return fmt.Sprintf("[data]\ndata_dir=%q\n", t.TempDir()) + profile
}

func TestMessengerBroadcastDefaults(t *testing.T) {
	assertions := assert.New(t)

	cfg := loadConfigText(t, broadcastConfigText(t, broadcastProfile))
	p := cfg.GetMessengerBroadcastProfile("EXAMPLE-OFFERS")
	require.NotNil(t, p)
	assertions.Equal(100, p.MaxRecipients)
	assertions.Equal(24, p.MinIntervalHours)
	assertions.Equal(24, p.ConsentMaxAgeHours)
	assertions.Equal(7, p.CampaignDays)
	assertions.True(p.Enabled)
	assertions.Empty(p.Actions)
	assertions.False(p.ActionAllowed("send"))
	assertions.Len(cfg.ScheduledMessengerBroadcastProfiles(), 1)
	assertions.Nil(cfg.GetMessengerBroadcastProfile("missing"))
	for _, extra := range []string{"enabled=false\n", "schedule=\"\"\n"} {
		profile := broadcastProfile
		if strings.HasPrefix(extra, "schedule") {
			profile = strings.ReplaceAll(profile, `schedule="17 */6 * * *"`, "")
		}
		cfg = loadConfigText(t, broadcastConfigText(t, profile+extra))
		assertions.Empty(cfg.ScheduledMessengerBroadcastProfiles())
	}
	cfg = loadConfigText(t, broadcastConfigText(t, broadcastProfile+`actions=["send"]
max_daily_budget_cents=500
max_recipients=1000
min_interval_hours=12
consent_max_age_hours=48
campaign_days=30
`))
	assertions.True(cfg.MessengerBroadcasts[0].ActionAllowed("send"))
	assertions.Equal(1000, cfg.MessengerBroadcasts[0].MaxRecipients)
}

func TestMessengerBroadcastRejects(t *testing.T) {
	for _, tc := range []struct{ old, replacement string }{
		{`identifier="example-offers"`, `identifier=""`},
		{`app_id="9"`, `app_id="9/path"`},
		{`page_id="100"`, `page_id="-100"`},
		{`ad_account_id="700"`, `ad_account_id="act_700"`},
		{`token_env="EXAMPLE_PAGE_TOKEN"`, `token_env="TOKEN=value"`},
		{`ad_token_env="EXAMPLE_AD_TOKEN"`, `ad_token_env=""`},
		{`schedule="17 */6 * * *"`, `schedule="tomorrow"`},
		{"", "max_recipients=1001\n"}, {"", "max_recipients=0\n"},
		{"", "min_interval_hours=11\n"}, {"", "consent_max_age_hours=0\n"},
		{"", "campaign_days=0\n"}, {"", "campaign_days=31\n"},
		{"", "max_daily_budget_cents=-1\n"},
		{"", "actions=[\"send\"]\n"},
		{"", "actions=[\"unknown\"]\n"},
		{"", "actions=[\"send\",\"send\"]\nmax_daily_budget_cents=500\n"},
		{"", "token=\"inline-secret\"\n"}, {"", "ad_token=\"inline-secret\"\n"},
	} {
		t.Run(tc.replacement, func(t *testing.T) {
			profile := broadcastProfile + tc.replacement
			if tc.old != "" {
				profile = strings.Replace(broadcastProfile, tc.old, tc.replacement, 1)
			}
			require.Error(t, loadConfigTextError(t, broadcastConfigText(t, profile)))
		})
	}
	other := strings.Replace(broadcastProfile, `page_id="100"`, `page_id="101"`, 1)
	other = strings.Replace(other, `identifier="example-offers"`, `identifier="EXAMPLE-OFFERS"`, 1)
	require.Error(t, loadConfigTextError(t, broadcastConfigText(t, broadcastProfile+other)))
	other = strings.Replace(broadcastProfile, `identifier="example-offers"`, `identifier="other"`, 1)
	require.Error(t, loadConfigTextError(t, broadcastConfigText(t, broadcastProfile+other)))
}
