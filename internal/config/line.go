package config

import (
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/envutil"
)

// LineSource configures a push source. The secret itself remains in the environment.
type LineSource struct {
	ChannelID string `toml:"channel_id"`
	SecretEnv string `toml:"secret_env"`
	Enabled   bool   `toml:"enabled"`
}

func (c *Config) GetLineSource(channelID string) *LineSource {
	for _, source := range c.Line {
		if source.ChannelID == channelID {
			return &source
		}
	}
	return nil
}

func (c *Config) validateLineSources() error {
	seen := make(map[string]bool)
	for index := range c.Line {
		source := &c.Line[index]
		source.ChannelID = strings.TrimSpace(source.ChannelID)
		if !ValidLineChannelID(source.ChannelID) {
			return fmt.Errorf("line[%d].channel_id must be a canonical positive decimal uint64", index)
		}
		if seen[source.ChannelID] {
			return fmt.Errorf("line[%d].channel_id is duplicate", index)
		}
		seen[source.ChannelID] = true
		source.SecretEnv = strings.TrimSpace(source.SecretEnv)
		if !envutil.ValidName(source.SecretEnv) {
			return fmt.Errorf("line[%d].secret_env must name an explicit environment variable", index)
		}
	}
	return nil
}

// ValidLineChannelID reports whether value is a canonical positive decimal
// uint64, the only form a LINE channel ID takes in config, CLI and receiver.
func ValidLineChannelID(value string) bool {
	id, err := strconv.ParseUint(value, 10, 64)
	return err == nil && id > 0 && strconv.FormatUint(id, 10) == value
}
