package config

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// BlandSource archives existing calls with an org API key. It never requests
// creation or resend endpoints. Optional correction retrieval needs explicit opt-in.
type BlandSource struct {
	FetchCorrectedTranscript bool   `toml:"fetch_corrected_transcript"`
	Identifier               string `toml:"identifier"`
	AccountEmail             string `toml:"account_email"`
	APIKey                   string `toml:"api_key"`
	EncryptedKey             string `toml:"encrypted_key"`
	Schedule                 string `toml:"schedule"`
	Enabled                  bool   `toml:"enabled"`
	MediaScope               string `toml:"media_scope"`
	MaxMediaMB               int    `toml:"max_media_mb"`
}

func (s BlandSource) EffectiveAccountEmail() (string, error) {
	return effectiveMeetingAccountEmail("bland", s.Identifier, s.AccountEmail)
}
func (s BlandSource) MediaPolicy() attachmentpolicy.Policy {
	maxBytes := attachmentpolicy.DefaultChatMaxBytes
	if s.MaxMediaMB > 0 {
		maxBytes = int64(s.MaxMediaMB) << 20
	}
	return attachmentpolicy.Policy{Scope: attachmentpolicy.Scope(s.MediaScope), MaxBytes: maxBytes}
}
func (c *Config) GetBlandSource(id string) *BlandSource {
	for _, s := range c.Bland {
		if strings.EqualFold(s.Identifier, id) {
			return &s
		}
	}
	return nil
}
func (c *Config) ScheduledBlandSources() []BlandSource {
	var out []BlandSource
	for _, s := range c.Bland {
		if s.Enabled && s.Schedule != "" {
			out = append(out, s)
		}
	}
	return out
}
func (c *Config) validateBlandSources() error {
	seen := map[string]bool{}
	for i := range c.Bland {
		s := &c.Bland[i]
		id := strings.ToLower(strings.TrimSpace(s.Identifier))
		if id == "" {
			return errors.New("[[bland]]: every entry needs an identifier")
		}
		if seen[id] {
			return fmt.Errorf("[[bland]]: duplicate identifier %q", s.Identifier)
		}
		seen[id] = true
		email, err := s.EffectiveAccountEmail()
		if err != nil {
			return err
		}
		s.AccountEmail = email
		if s.MaxMediaMB < 0 || int64(s.MaxMediaMB) > int64(^uint64(0)>>1)>>20 {
			return errors.New("[[bland]]: invalid max_media_mb")
		}
		if err := s.MediaPolicy().Validate(); err != nil {
			return err
		}
	}
	return nil
}
