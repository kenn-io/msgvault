package config

import (
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// BlandSource archives existing calls with an org API key. It never requests
// creation or resend endpoints. EncryptedKey is the key for your own Twilio
// account; with it, Bland returns only inbound numbers associated with that
// account SID.
type BlandSource struct {
	Identifier   string `toml:"identifier"`
	AccountEmail string `toml:"account_email"`
	APIKey       string `toml:"api_key"`
	EncryptedKey string `toml:"encrypted_key"`
	Schedule     string `toml:"schedule"`
	Enabled      bool   `toml:"enabled"`
	Media        *bool  `toml:"media"`
	MaxMediaMB   int    `toml:"max_media_mb"`
}

func (s BlandSource) EffectiveAccountEmail() (string, error) {
	return effectiveMeetingAccountEmail("bland", s.Identifier, s.AccountEmail)
}

// MediaPolicy caps each recording at the shared chat default unless
// max_media_mb is set.
func (s BlandSource) MediaPolicy() attachmentpolicy.Policy {
	return resolveMediaPolicy(s.Media, "", 0, s.MaxMediaMB, attachmentpolicy.DefaultChatMaxBytes, MediaAccountConfig{}, false)
}

// GetBlandSource returns the configured Bland source matching identifier
// (case-insensitive), or nil.
func (c *Config) GetBlandSource(identifier string) *BlandSource {
	for _, src := range c.Bland {
		if strings.EqualFold(src.Identifier, identifier) {
			cp := src
			return &cp
		}
	}
	return nil
}

// ScheduledBlandSources returns enabled Bland sources with a cron schedule.
func (c *Config) ScheduledBlandSources() []BlandSource {
	var out []BlandSource
	for _, src := range c.Bland {
		if src.Enabled && src.Schedule != "" {
			out = append(out, src)
		}
	}
	return out
}
