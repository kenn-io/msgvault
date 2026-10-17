package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoopsConfigDefaultsAndManualSchedule(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	cfg := NewDefaultConfig()
	assertions.True(cfg.Loops.Enabled)
	assertions.Equal("*/15 * * * *", cfg.Loops.Schedule)
	assertions.Equal(24*time.Hour, cfg.Loops.ReplyOwedAfter)
	assertions.Equal(72*time.Hour, cfg.Loops.AwaitingReplyAfter)
	assertions.Equal(365*24*time.Hour, cfg.Loops.Lookback)
	assertions.Equal(12, cfg.Loops.RunMessageLimit)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[loops]\nenabled = false\nschedule = \"\"\nlookback = \"0s\"\nreply_owed_after = \"12h\"\n"), 0600))
	loaded, err := Load(path, "")
	requirements.NoError(err)
	assertions.False(loaded.Loops.Enabled)
	assertions.Empty(loaded.Loops.Schedule)
	assertions.Zero(loaded.Loops.Lookback)
	assertions.Equal(12*time.Hour, loaded.Loops.ReplyOwedAfter)
	assertions.Equal(72*time.Hour, loaded.Loops.AwaitingReplyAfter)
}

func TestLoopsConfigValidation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*LoopsConfig)
	}{
		{"zero reply threshold", func(c *LoopsConfig) { c.ReplyOwedAfter = 0 }},
		{"negative awaiting threshold", func(c *LoopsConfig) { c.AwaitingReplyAfter = -time.Hour }},
		{"zero freshness threshold", func(c *LoopsConfig) { c.StaleAfter = 0 }},
		{"negative lookback", func(c *LoopsConfig) { c.Lookback = -time.Hour }},
		{"zero reconciliation interval", func(c *LoopsConfig) { c.ReconcileInterval = 0 }},
		{"empty group", func(c *LoopsConfig) { c.MaxGroupCounterparts = 0 }},
		{"zero feed pages", func(c *LoopsConfig) { c.FeedPages = 0 }},
		{"zero batch messages", func(c *LoopsConfig) { c.BatchMessages = 0 }},
		{"zero batch conversations", func(c *LoopsConfig) { c.BatchConversations = 0 }},
		{"zero rescan batch", func(c *LoopsConfig) { c.RescanBatch = 0 }},
		{"zero backfill batch", func(c *LoopsConfig) { c.SignalsBackfillBatch = 0 }},
		{"body limit exceeds conversation", func(c *LoopsConfig) { c.RunMessageLimit = c.ConversationMessageLimit + 1 }},
		{"extended limit below normal", func(c *LoopsConfig) { c.ConversationMessageLimitExtended = c.ConversationMessageLimit - 1 }},
		{"six cron fields", func(c *LoopsConfig) { c.Schedule = "* * * * * *" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := NewDefaultConfig().Loops
			tt.change(&c)
			require.ErrorContains(t, c.Validate(), "[loops]")
		})
	}
}

func TestLoopsConfigRejectsExplicitZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[loops]\nreply_owed_after = \"0s\"\n"), 0600))
	_, err := Load(path, "")
	require.ErrorContains(t, err, "reply_owed_after")
}
