package config

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// LoopsConfig bounds deterministic derivation work and its freshness policy.
// Load starts with DefaultLoopsConfig before TOML decoding so explicit false,
// an empty schedule and a zero lookback retain their meanings.
type LoopsConfig struct {
	Enabled                          bool          `toml:"enabled"`
	Schedule                         string        `toml:"schedule"`
	ReplyOwedAfter                   time.Duration `toml:"reply_owed_after"`
	AwaitingReplyAfter               time.Duration `toml:"awaiting_reply_after"`
	StaleAfter                       time.Duration `toml:"stale_after"`
	MaxGroupCounterparts             int           `toml:"max_group_counterparts"`
	Lookback                         time.Duration `toml:"lookback"`
	ReconcileInterval                time.Duration `toml:"reconcile_interval"`
	FeedPages                        int           `toml:"feed_pages"`
	BatchMessages                    int           `toml:"batch_messages"`
	BatchConversations               int           `toml:"batch_conversations"`
	RescanBatch                      int           `toml:"rescan_batch"`
	SignalsBackfillBatch             int           `toml:"signals_backfill_batch"`
	ConversationMessageLimit         int           `toml:"conversation_message_limit"`
	ConversationMessageLimitExtended int           `toml:"conversation_message_limit_extended"`
	RunMessageLimit                  int           `toml:"run_message_limit"`
}

func DefaultLoopsConfig() LoopsConfig {
	return LoopsConfig{
		Enabled: true, Schedule: "*/15 * * * *", ReplyOwedAfter: 24 * time.Hour, AwaitingReplyAfter: 72 * time.Hour,
		StaleAfter: 24 * time.Hour, MaxGroupCounterparts: 3, Lookback: 365 * 24 * time.Hour, ReconcileInterval: 24 * time.Hour,
		FeedPages: 5, BatchMessages: 2000, BatchConversations: 200, RescanBatch: 500, SignalsBackfillBatch: 2000,
		ConversationMessageLimit: 200, ConversationMessageLimitExtended: 2000, RunMessageLimit: 12,
	}
}

func (c LoopsConfig) Validate() error {
	for _, v := range []struct {
		name  string
		value time.Duration
	}{
		{"reply_owed_after", c.ReplyOwedAfter}, {"awaiting_reply_after", c.AwaitingReplyAfter},
		{"stale_after", c.StaleAfter}, {"reconcile_interval", c.ReconcileInterval},
	} {
		if v.value <= 0 {
			return fmt.Errorf("invalid [loops] %s: must be positive", v.name)
		}
	}
	if c.Lookback < 0 {
		return fmt.Errorf("invalid [loops] lookback: must be nonnegative")
	}
	for _, v := range []struct {
		name  string
		value int
	}{
		{"max_group_counterparts", c.MaxGroupCounterparts}, {"feed_pages", c.FeedPages},
		{"batch_messages", c.BatchMessages}, {"batch_conversations", c.BatchConversations},
		{"rescan_batch", c.RescanBatch}, {"signals_backfill_batch", c.SignalsBackfillBatch},
		{"conversation_message_limit", c.ConversationMessageLimit},
		{"conversation_message_limit_extended", c.ConversationMessageLimitExtended}, {"run_message_limit", c.RunMessageLimit},
	} {
		if v.value < 1 {
			return fmt.Errorf("invalid [loops] %s: must be at least 1", v.name)
		}
	}
	if c.RunMessageLimit > c.ConversationMessageLimit {
		return fmt.Errorf("invalid [loops] run_message_limit: exceeds conversation_message_limit")
	}
	if c.ConversationMessageLimitExtended < c.ConversationMessageLimit {
		return fmt.Errorf("invalid [loops] conversation_message_limit_extended: below conversation_message_limit")
	}
	if c.Schedule != "" {
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		if _, err := parser.Parse(c.Schedule); err != nil {
			return fmt.Errorf("invalid [loops] schedule: %w", err)
		}
	}
	return nil
}
