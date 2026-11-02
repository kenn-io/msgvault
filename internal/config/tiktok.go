package config

import (
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/cronexpr"
	"go.kenn.io/msgvault/internal/envutil"
	"go.kenn.io/msgvault/internal/tiktok"
)

// TikTokSource selects one authorized account. Secrets are read only by the
// daemon from the named environment variables, never from this profile.
type TikTokSource struct {
	Identifier   string `toml:"identifier"`
	AppID        string `toml:"app_id"`
	AccountID    string `toml:"account_id"`
	TokenEnv     string `toml:"token_env"`
	AppSecretEnv string `toml:"app_secret_env"`
	Enabled      bool   `toml:"enabled"`
	Schedule     string `toml:"schedule"`
}

func (c *Config) GetTikTokSource(identifier string) *TikTokSource {
	for _, source := range c.TikTok {
		if strings.EqualFold(source.Identifier, identifier) {
			return &source
		}
	}
	return nil
}
func (c *Config) ScheduledTikTokSources() []TikTokSource {
	var sources []TikTokSource
	for _, source := range c.TikTok {
		if source.Enabled && source.Schedule != "" {
			sources = append(sources, source)
		}
	}
	return sources
}
func (c *Config) validateTikTokSources() error {
	names, accounts := map[string]bool{}, map[string]bool{}
	for i := range c.TikTok {
		s := &c.TikTok[i]
		s.Identifier = strings.TrimSpace(s.Identifier)
		name := strings.ToLower(s.Identifier)
		if name == "" || names[name] {
			return fmt.Errorf("tiktok[%d].identifier is empty or duplicate", i)
		}
		names[name] = true
		if !tiktok.ValidID(s.AppID) || !tiktok.ValidID(s.AccountID) {
			return fmt.Errorf("tiktok[%d] requires app_id and account_id (OAuth open_id)", i)
		}
		key := tiktok.SourceIdentifier(s.AppID, s.AccountID)
		if accounts[key] {
			return fmt.Errorf("tiktok[%d] duplicates an app/account pair", i)
		}
		accounts[key] = true
		if !envutil.ValidName(s.TokenEnv) {
			return fmt.Errorf("tiktok[%d].token_env must name an explicit environment variable", i)
		}
		if s.AppSecretEnv != "" && !envutil.ValidName(s.AppSecretEnv) {
			return fmt.Errorf("tiktok[%d].app_secret_env must name an environment variable", i)
		}
		if s.Schedule != "" {
			if _, err := cronexpr.Parser.Parse(s.Schedule); err != nil {
				return fmt.Errorf("tiktok[%d].schedule: %w", i, err)
			}
		}
	}
	return nil
}
