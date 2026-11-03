package config

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
	"go.kenn.io/msgvault/internal/cronexpr"
	"go.kenn.io/msgvault/internal/envutil"
	"go.kenn.io/msgvault/internal/meta"
)

// MessengerBroadcastProfile binds consent and spend limits to a Messenger Page.
// Credentials are resolved only from these named daemon environment variables.
type MessengerBroadcastProfile struct {
	Identifier          string   `toml:"identifier"`
	AppID               string   `toml:"app_id"`
	PageID              string   `toml:"page_id"`
	AdAccountID         string   `toml:"ad_account_id"`
	TokenEnv            string   `toml:"token_env"`
	AdTokenEnv          string   `toml:"ad_token_env"`
	Enabled             bool     `toml:"enabled"`
	Actions             []string `toml:"actions"`
	MaxDailyBudgetCents int64    `toml:"max_daily_budget_cents"`
	MaxRecipients       int      `toml:"max_recipients"`
	MinIntervalHours    int      `toml:"min_interval_hours"`
	ConsentMaxAgeHours  int      `toml:"consent_max_age_hours"`
	CampaignDays        int      `toml:"campaign_days"`
	Schedule            string   `toml:"schedule"`
}

func (p MessengerBroadcastProfile) ActionAllowed(action string) bool {
	return ActionsAllow(p.Actions, action)
}
func (c *Config) GetMessengerBroadcastProfile(id string) *MessengerBroadcastProfile {
	for _, p := range c.MessengerBroadcasts {
		if strings.EqualFold(p.Identifier, id) {
			return &p
		}
	}
	return nil
}
func (c *Config) ScheduledMessengerBroadcastProfiles() []MessengerBroadcastProfile {
	var out []MessengerBroadcastProfile
	for _, p := range c.MessengerBroadcasts {
		if p.Enabled && p.Schedule != "" {
			out = append(out, p)
		}
	}
	return out
}

// A presence-only decode preserves explicit false and rejects explicit zero
// limits. The main decode still owns unknown-key detection.
func (c *Config) applyMessengerBroadcastDefaults(content []byte) error {
	var raw struct {
		Profiles []struct {
			Enabled            *bool `toml:"enabled"`
			MaxRecipients      *int  `toml:"max_recipients"`
			MinIntervalHours   *int  `toml:"min_interval_hours"`
			ConsentMaxAgeHours *int  `toml:"consent_max_age_hours"`
			CampaignDays       *int  `toml:"campaign_days"`
		} `toml:"messenger_broadcasts"`
	}
	if _, err := toml.Decode(string(content), &raw); err != nil {
		return fmt.Errorf("decode broadcast defaults: %w", err)
	}
	for i, p := range raw.Profiles {
		dst := &c.MessengerBroadcasts[i]
		if p.Enabled == nil {
			dst.Enabled = true
		}
		if p.MaxRecipients == nil {
			dst.MaxRecipients = 100
		}
		if p.MinIntervalHours == nil {
			dst.MinIntervalHours = 24
		}
		if p.ConsentMaxAgeHours == nil {
			dst.ConsentMaxAgeHours = 24
		}
		if p.CampaignDays == nil {
			dst.CampaignDays = 7
		}
	}
	return nil
}

func (c *Config) validateMessengerBroadcastProfiles() error {
	assets := map[string]bool{}
	for i := range c.MessengerBroadcasts {
		p := &c.MessengerBroadcasts[i]
		p.Identifier = strings.TrimSpace(p.Identifier)
		if p.Identifier == "" {
			return fmt.Errorf("messenger_broadcasts[%d].identifier is empty or duplicate", i)
		}
		for _, prior := range c.MessengerBroadcasts[:i] {
			if strings.EqualFold(prior.Identifier, p.Identifier) {
				return fmt.Errorf("messenger_broadcasts[%d].identifier is empty or duplicate", i)
			}
		}
		if !meta.ValidAccountID(p.AppID) || !meta.ValidAccountID(p.PageID) || !meta.ValidAccountID(p.AdAccountID) {
			return fmt.Errorf("messenger_broadcasts[%d] requires decimal app_id, page_id and ad_account_id without act_", i)
		}
		key := meta.SourceIdentifier(p.AppID, p.PageID)
		if assets[key] {
			return fmt.Errorf("messenger_broadcasts[%d] duplicates an app/Page pair", i)
		}
		assets[key] = true
		if !envutil.ValidName(p.TokenEnv) || !envutil.ValidName(p.AdTokenEnv) {
			return fmt.Errorf("messenger_broadcasts[%d].token_env and ad_token_env must name explicit environment variables", i)
		}
		if p.MaxRecipients < 1 || p.MaxRecipients > 1000 {
			return fmt.Errorf("messenger_broadcasts[%d].max_recipients must be between 1 and 1000", i)
		}
		if p.MinIntervalHours < 12 {
			return fmt.Errorf("messenger_broadcasts[%d].min_interval_hours must be at least 12", i)
		}
		if p.ConsentMaxAgeHours < 1 {
			return fmt.Errorf("messenger_broadcasts[%d].consent_max_age_hours must be positive", i)
		}
		if p.CampaignDays < 1 || p.CampaignDays > 30 {
			return fmt.Errorf("messenger_broadcasts[%d].campaign_days must be between 1 and 30", i)
		}
		if err := ValidateMetaActions("messenger_broadcasts", i, p.Actions, "send"); err != nil {
			return err
		}
		if p.MaxDailyBudgetCents < 0 || (p.ActionAllowed("send") && p.MaxDailyBudgetCents == 0) {
			return fmt.Errorf("messenger_broadcasts[%d].max_daily_budget_cents must be positive when send is allowed", i)
		}
		if p.Schedule != "" {
			if _, err := cronexpr.Parser.Parse(p.Schedule); err != nil {
				return fmt.Errorf("messenger_broadcasts[%d].schedule: %w", i, err)
			}
		}
	}
	return nil
}
