package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTikTokProfiles(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	valid := TikTokSource{Identifier: "Example", AppID: "app-example", AccountID: "account+example/", TokenEnv: "EXAMPLE_TIKTOK_TOKEN", Enabled: true, Schedule: "*/30 * * * *"}
	cfg := NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.TikTok = []TikTokSource{valid}
	requirements.NoError(cfg.validateTikTokSources())
	assertions.Equal(&valid, cfg.GetTikTokSource("example"))
	assertions.Len(cfg.ScheduledTikTokSources(), 1)
	for _, tc := range []struct {
		name   string
		modify func(*TikTokSource)
	}{
		{"identifier", func(s *TikTokSource) { s.Identifier = " " }},
		{"account", func(s *TikTokSource) { s.AccountID = "" }},
		{"token", func(s *TikTokSource) { s.TokenEnv = "a token" }},
		{"secret", func(s *TikTokSource) { s.AppSecretEnv = "bad name" }},
		{"schedule", func(s *TikTokSource) { s.Schedule = "not cron" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg.TikTok = []TikTokSource{valid}
			tc.modify(&cfg.TikTok[0])
			require.Error(t, cfg.validateTikTokSources())
		})
	}
	cfg.TikTok = []TikTokSource{valid, valid}
	requirements.Error(cfg.validateTikTokSources())
}
