package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// HexSource describes a recorder-local schema-1 meeting folder.
type HexSource struct {
	Identifier        string `toml:"identifier"`
	AccountEmail      string `toml:"account_email"`
	MeetingsDir       string `toml:"meetings_dir"`
	Schedule          string `toml:"schedule"`
	Enabled           bool   `toml:"enabled"`
	Recordings        bool   `toml:"recordings"`
	RecordingMaxBytes int64  `toml:"recording_max_bytes"`
}

func (s HexSource) EffectiveAccountEmail() (string, error) {
	return effectiveMeetingAccountEmail("hex", s.Identifier, s.AccountEmail)
}
func (s HexSource) EffectiveMeetingsDir() string {
	if s.MeetingsDir != "" {
		return s.MeetingsDir
	}
	return DefaultHexMeetingsDir()
}
func (s HexSource) EffectiveRecordingMaxBytes() int64 {
	if s.RecordingMaxBytes > 0 {
		return s.RecordingMaxBytes
	}
	return DefaultMeetingRecordingMaxBytes
}
func (c *Config) GetHexSource(identifier string) *HexSource {
	for _, src := range c.Hex {
		if strings.EqualFold(src.Identifier, identifier) {
			return &src
		}
	}
	return nil
}
func (c *Config) ScheduledHexSources() []HexSource {
	var out []HexSource
	for _, src := range c.Hex {
		if src.Enabled && src.Schedule != "" {
			out = append(out, src)
		}
	}
	return out
}

func DefaultHexMeetingsDir() string {
	if root := os.Getenv("HEX_APPLICATION_SUPPORT_DIR"); root != "" {
		return filepath.Join(root, "meetings")
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "hex", "meetings")
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "hex", "meetings")
	default:
		root := os.Getenv("XDG_DATA_HOME")
		if root == "" {
			root = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(root, "hex", "meetings")
	}
}
