package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/looplimits"
)

func TestLoadLoopsConfigDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[loops]\n"), 0600))
	loaded, err := Load(path, "")
	require.NoError(t, err)

	assert.True(t, loaded.Loops.Enabled)
	assert.Equal(t, "*/15 * * * *", loaded.Loops.Schedule)
	assert.Equal(t, 24*time.Hour, loaded.Loops.ReplyOwedAfter)
	assert.Equal(t, 72*time.Hour, loaded.Loops.AwaitingReplyAfter)
	assert.Equal(t, 24*time.Hour, loaded.Loops.StaleAfter)
	assert.Equal(t, 3, loaded.Loops.MaxGroupCounterparts)
	assert.Equal(t, 365*24*time.Hour, loaded.Loops.Lookback)
	assert.Equal(t, 24*time.Hour, loaded.Loops.ReconcileInterval)
	assert.Equal(t, 5, loaded.Loops.FeedPages)
	assert.Equal(t, 2000, loaded.Loops.BatchMessages)
	assert.Equal(t, 200, loaded.Loops.BatchConversations)
	assert.Equal(t, 500, loaded.Loops.RescanBatch)
	assert.Equal(t, 2000, loaded.Loops.SignalsBackfillBatch)
	assert.Equal(t, 200, loaded.Loops.ConversationMessageLimit)
	assert.Equal(t, 2000, loaded.Loops.ConversationMessageLimitExtended)
	assert.Equal(t, 12, loaded.Loops.RunMessageLimit)
}

func TestLoopsConfigLoadAndManualSchedule(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
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

func TestLoopsConfigRejectsConversationLimitsAboveLoaderMaximum(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*LoopsConfig)
	}{
		{"normal limit", func(c *LoopsConfig) {
			c.ConversationMessageLimit = looplimits.MaxConversationMessageLimit + 1
			c.ConversationMessageLimitExtended = looplimits.MaxConversationMessageLimit + 1
		}},
		{"extended limit", func(c *LoopsConfig) {
			c.ConversationMessageLimitExtended = looplimits.MaxConversationMessageLimit + 1
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultLoopsConfig()
			tt.change(&cfg)
			require.ErrorContains(t, cfg.Validate(), "conversation_message_limit")
		})
	}
}
