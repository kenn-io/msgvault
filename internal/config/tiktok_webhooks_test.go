package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTikTokWebhookConfiguration(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	valid := TikTokWebhooksConfig{ListenAddr: "127.0.0.1:8788", Apps: []TikTokWebhookApp{{AppID: "100001", AppSecretEnv: "EXAMPLE_TIKTOK_SECRET"}}}
	requirements.NoError(valid.Validate())
	requirements.NoError((TikTokWebhooksConfig{}).Validate())
	for _, tc := range []struct {
		name   string
		change func(*TikTokWebhooksConfig)
	}{
		{"body", func(c *TikTokWebhooksConfig) { c.MaxBodyBytes = -1 }},
		{"spool", func(c *TikTokWebhooksConfig) { c.MaxSpoolBytes = 1024 }},
		{"retention", func(c *TikTokWebhooksConfig) { c.DoneRetention = "invalid" }},
		{"address", func(c *TikTokWebhooksConfig) { c.ListenAddr = "missing-port" }},
		{"apps", func(c *TikTokWebhooksConfig) { c.Apps = nil }},
		{"secret", func(c *TikTokWebhooksConfig) {
			c.Apps = []TikTokWebhookApp{{AppID: "100001", AppSecretEnv: "bad name"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { c := valid; tc.change(&c); require.Error(t, c.Validate()) })
	}
	cfg := NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.TikTok = []TikTokSource{{Identifier: "messages", Enabled: true, CommentsEnabled: new(false), MessagesEnabled: true, Schedule: "*/30 * * * *"}}
	assertions.Empty(cfg.ScheduledTikTokSources())
	assertions.Len(cfg.ScheduledTikTokMessageSources(), 1)
}
