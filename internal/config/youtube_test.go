package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestYouTubeProfileValidation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	const validProfile = `[[youtube]]
identifier="fixture"
channel_id="UCfixture"
token_env="FIXTURE_REFRESH"
client_id_env="FIXTURE_CLIENT"
client_secret_env="FIXTURE_SECRET"
enabled=true
schedule="*/30 * * * *"
`
	cfg := loadConfigText(t, validProfile)
	requirements.Len(cfg.YouTube, 1)
	assertions.Equal("UCfixture", cfg.GetYouTubeSource("FIXTURE").ChannelID)
	assertions.Len(cfg.ScheduledYouTubeSources(), 1)
	for _, tc := range []struct {
		name, profile, wantError string
	}{
		{
			name:      "invalid token environment name",
			profile:   strings.Replace(validProfile, `token_env="FIXTURE_REFRESH"`, `token_env="secret value"`, 1),
			wantError: "youtube[0].token_env must name an environment variable",
		},
		{
			name:      "several invalid environment names",
			profile:   strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(validProfile, `token_env="FIXTURE_REFRESH"`, `token_env="bad value"`), `client_id_env="FIXTURE_CLIENT"`, `client_id_env="bad value"`), `client_secret_env="FIXTURE_SECRET"`, `client_secret_env="bad value"`),
			wantError: "youtube[0].token_env must name an environment variable",
		},
		{
			name:      "invalid channel ID",
			profile:   strings.Replace(validProfile, `channel_id="UCfixture"`, `channel_id="https://example.test"`, 1),
			wantError: "youtube[0].channel_id invalid or duplicate",
		},
		{
			name:      "duplicate identifier by case fold",
			profile:   strings.Replace(validProfile, `identifier="fixture"`, `identifier="Σ"`, 1) + strings.Replace(strings.Replace(validProfile, `identifier="fixture"`, `identifier="ς"`, 1), `channel_id="UCfixture"`, `channel_id="UCsecond"`, 1),
			wantError: "youtube[1].identifier missing or duplicate",
		},
		{
			name:      "duplicate channel ID",
			profile:   validProfile + strings.Replace(validProfile, `identifier="fixture"`, `identifier="second"`, 1),
			wantError: "youtube[1].channel_id invalid or duplicate",
		},
		{
			name:      "unknown key",
			profile:   strings.Replace(validProfile, "enabled=true", "token='do-not-store'\nenabled=true", 1),
			wantError: `unknown YouTube config key "youtube.token"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, loadConfigTextError(t, tc.profile), tc.wantError)
		})
	}
}
