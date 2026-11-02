package config

import (
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/envutil"
)

// LineSource configures a push source. The secret itself remains in the environment.
type LineSource struct {
	ChannelID  string `toml:"channel_id"`
	SecretEnv  string `toml:"secret_env"`
	Enabled    bool   `toml:"enabled"`
	Media      *bool  `toml:"media"`
	MaxMediaMB int64  `toml:"max_media_mb"`
	AllowSend  bool   `toml:"allow_send"`
}

func (s LineSource) MediaPolicy() attachmentpolicy.Policy {
	return resolveMediaPolicy(nil, "", 0, int(s.MaxMediaMB), attachmentpolicy.DefaultChatMaxBytes, MediaAccountConfig{Media: s.Media}, true)
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
		if !ValidLineChannelID(source.ChannelID) {
			return fmt.Errorf("line[%d].channel_id: 1–18446744073709551615, no leading zeros", index)
		}
		if seen[source.ChannelID] {
			return fmt.Errorf("line[%d].channel_id is duplicate", index)
		}
		if !validMaxMediaMB(source.MaxMediaMB) {
			return fmt.Errorf("line[%d].max_media_mb: 0–%d", index, maxMediaMBLimit)
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
