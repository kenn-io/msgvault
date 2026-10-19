package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/looplimits"
)

func TestLoadLoopsConfigDefaults(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[loops]\n"), 0600))
	loaded, err := Load(path, "")
	requirements.NoError(err)

	assertions.True(loaded.Loops.Enabled)
	assertions.Equal("*/15 * * * *", loaded.Loops.Schedule)
	assertions.Equal(24*time.Hour, loaded.Loops.ReplyOwedAfter)
	assertions.Equal(72*time.Hour, loaded.Loops.AwaitingReplyAfter)
	assertions.Equal(24*time.Hour, loaded.Loops.StaleAfter)
	assertions.Equal(3, loaded.Loops.MaxGroupCounterparts)
	assertions.Equal(365*24*time.Hour, loaded.Loops.Lookback)
	assertions.Equal(24*time.Hour, loaded.Loops.ReconcileInterval)
	assertions.Equal(5, loaded.Loops.FeedPages)
	assertions.Equal(2000, loaded.Loops.BatchMessages)
	assertions.Equal(200, loaded.Loops.BatchConversations)
	assertions.Equal(500, loaded.Loops.RescanBatch)
	assertions.Equal(2000, loaded.Loops.SignalsBackfillBatch)
	assertions.Equal(200, loaded.Loops.ConversationMessageLimit)
	assertions.Equal(2000, loaded.Loops.ConversationMessageLimitExtended)
	assertions.Equal(12, loaded.Loops.RunMessageLimit)
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
}

func TestLoopsConfigValidation(t *testing.T) {
	wantFields := map[string]string{
		"conversation message limit too large":          "conversation_message_limit",
		"extended conversation message limit too large": "conversation_message_limit",
	}
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
		{"conversation message limit too large", func(c *LoopsConfig) { c.ConversationMessageLimit = looplimits.MaxConversationMessageLimit + 1 }},
		{"extended conversation message limit too large", func(c *LoopsConfig) { c.ConversationMessageLimitExtended = looplimits.MaxConversationMessageLimit + 1 }},
		{"six cron fields", func(c *LoopsConfig) { c.Schedule = "* * * * * *" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := NewDefaultConfig().Loops
			tt.change(&c)
			err := c.Validate()
			require.ErrorContains(t, err, "[loops]")
			if field := wantFields[tt.name]; field != "" {
				require.ErrorContains(t, err, field)
			}
		})
	}
}

func TestLoopsConfigRejectsExplicitZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[loops]\nreply_owed_after = \"0s\"\n"), 0600))
	_, err := Load(path, "")
	require.ErrorContains(t, err, "reply_owed_after")
}

func TestLoopsKataDefaultsAndOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[loops.kata]\ncomment_on_close = false\nclose_on_issue_closed = false\nreopen_window = \"24h\"\nbatch = 3\n"), 0600))
	loaded, err := Load(path, "")
	require.NoError(t, err)
	assert.False(t, loaded.Loops.Kata.CommentOnClose)
	assert.False(t, loaded.Loops.Kata.CloseOnIssueClosed)
	assert.Equal(t, 24*time.Hour, loaded.Loops.Kata.ReopenWindow)
	assert.Equal(t, 3, loaded.Loops.Kata.Batch)
	for _, change := range []func(*LoopsConfig){func(c *LoopsConfig) { c.Kata.Batch = 0 }, func(c *LoopsConfig) { c.Kata.ReopenWindow = 0 }} {
		c := DefaultLoopsConfig()
		change(&c)
		require.ErrorContains(t, c.Validate(), "[loops.kata]")
	}
}

func TestLoadRejectsExtendedConversationMessageLimitAboveLoaderMaximum(t *testing.T) {
	for _, tc := range []struct {
		limit   int
		wantErr bool
	}{
		{limit: 10000},
		{limit: 10001, wantErr: true},
	} {
		t.Run(strconv.Itoa(tc.limit), func(t *testing.T) {
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			contents := []byte("[loops]\nconversation_message_limit_extended = " + strconv.Itoa(tc.limit) + "\n")
			requirements.NoError(os.WriteFile(path, contents, 0600))
			loaded, err := Load(path, "")
			if tc.wantErr {
				requirements.ErrorContains(err, "conversation_message_limit_extended")
				requirements.ErrorContains(err, "10000")
				return
			}
			requirements.NoError(err)
			requirements.Equal(tc.limit, loaded.Loops.ConversationMessageLimitExtended)
		})
	}
}
