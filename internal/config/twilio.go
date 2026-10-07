package config

import (
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// TwilioSource identifies one Twilio account or subaccount in one region.
// The daemon only reads existing calls, recordings, and retained transcripts.
type TwilioSource struct {
	Identifier             string `toml:"identifier"`
	AccountEmail           string `toml:"account_email"`
	AccountSID             string `toml:"account_sid"`
	APIKeySID              string `toml:"api_key_sid"`
	APIKeySecret           string `toml:"api_key_secret"`
	AuthToken              string `toml:"auth_token"`
	Region                 string `toml:"region"`
	IntelligenceServiceSID string `toml:"intelligence_service_sid"`
	Schedule               string `toml:"schedule"`
	Enabled                bool   `toml:"enabled"`
	Media                  *bool  `toml:"media"`
	MaxMediaMB             int    `toml:"max_media_mb"`
}

// EffectiveAccountEmail returns the configured primary account identity.
func (s TwilioSource) EffectiveAccountEmail() (string, error) {
	return effectiveMeetingAccountEmail("twilio", s.Identifier, s.AccountEmail)
}

// MediaPolicy caps each recording at the shared chat default unless
// max_media_mb is set.
func (s TwilioSource) MediaPolicy() attachmentpolicy.Policy {
	return resolveMediaPolicy(s.Media, "", 0, s.MaxMediaMB, attachmentpolicy.DefaultChatMaxBytes, MediaAccountConfig{}, false)
}

// GetTwilioSource returns the configured Twilio source matching identifier
// (case-insensitive), or nil.
func (c *Config) GetTwilioSource(identifier string) *TwilioSource {
	for _, src := range c.Twilio {
		if strings.EqualFold(src.Identifier, identifier) {
			cp := src
			return &cp
		}
	}
	return nil
}

// ScheduledTwilioSources returns enabled Twilio sources with a cron schedule.
func (c *Config) ScheduledTwilioSources() []TwilioSource {
	var out []TwilioSource
	for _, src := range c.Twilio {
		if src.Enabled && src.Schedule != "" {
			out = append(out, src)
		}
	}
	return out
}
