package config

import (
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/cronexpr"
	"go.kenn.io/msgvault/internal/envutil"
	"go.kenn.io/msgvault/internal/youtube"
)

type YouTubeSource struct {
	Identifier      string `toml:"identifier"`
	ChannelID       string `toml:"channel_id"`
	TokenEnv        string `toml:"token_env"`
	ClientIDEnv     string `toml:"client_id_env"`
	ClientSecretEnv string `toml:"client_secret_env"`
	Enabled         bool   `toml:"enabled"`
	Schedule        string `toml:"schedule"`
}

func (c *Config) GetYouTubeSource(identifier string) *YouTubeSource {
	for _, src := range c.YouTube {
		if strings.EqualFold(src.Identifier, identifier) {
			return &src
		}
	}
	return nil
}
func (c *Config) ScheduledYouTubeSources() []YouTubeSource {
	var out []YouTubeSource
	for _, src := range c.YouTube {
		if src.Enabled && src.Schedule != "" {
			out = append(out, src)
		}
	}
	return out
}
func (c *Config) validateYouTubeSources() error {
	var names []string
	channels := map[string]bool{}
	for i := range c.YouTube {
		src := &c.YouTube[i]
		src.Identifier = strings.TrimSpace(src.Identifier)
		src.ChannelID = strings.TrimSpace(src.ChannelID)
		name := src.Identifier
		duplicate := false
		for _, prior := range names {
			duplicate = duplicate || strings.EqualFold(name, prior)
		}
		if name == "" || duplicate {
			return fmt.Errorf("youtube[%d].identifier missing or duplicate", i)
		}
		names = append(names, name)
		if youtube.SourceIdentifier(src.ChannelID) == "" || channels[src.ChannelID] {
			return fmt.Errorf("youtube[%d].channel_id invalid or duplicate", i)
		}
		channels[src.ChannelID] = true
		for _, field := range []struct{ key, value string }{{"token_env", src.TokenEnv}, {"client_id_env", src.ClientIDEnv}, {"client_secret_env", src.ClientSecretEnv}} {
			if !envutil.ValidName(field.value) {
				return fmt.Errorf("youtube[%d].%s must name an environment variable", i, field.key)
			}
		}
		if src.Schedule != "" {
			if _, err := cronexpr.Parser.Parse(src.Schedule); err != nil {
				return fmt.Errorf("youtube[%d].schedule: %w", i, err)
			}
		}
	}
	return nil
}
