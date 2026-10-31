package config

import "strings"

// PocketSource selects a personal Pocket account. The daemon reads the key
// from APIKeyEnv; credentials are never written to the archive.
type PocketSource struct {
	Identifier   string `toml:"identifier"`
	AccountEmail string `toml:"account_email"`
	APIKeyEnv    string `toml:"api_key_env"`
	Schedule     string `toml:"schedule"`
	Enabled      bool   `toml:"enabled"`
}

func (s PocketSource) EffectiveAccountEmail() (string, error) {
	return effectiveMeetingAccountEmail("pocket", s.Identifier, s.AccountEmail)
}

func (c *Config) GetPocketSource(identifier string) *PocketSource {
	for _, src := range c.Pocket {
		if strings.EqualFold(src.Identifier, identifier) {
			return &src
		}
	}
	return nil
}

func (c *Config) ScheduledPocketSources() []PocketSource {
	var out []PocketSource
	for _, src := range c.Pocket {
		if src.Enabled && src.Schedule != "" {
			out = append(out, src)
		}
	}
	return out
}
