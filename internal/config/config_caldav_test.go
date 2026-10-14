package config

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCalDAVSources(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	t.Setenv("MSGVAULT_HOME", dir)
	path := filepath.Join(dir, "config.toml")
	requirements.NoError(os.WriteFile(path, []byte(`[[caldav.sources]]
name = "personal"
url = "https://calendar.example.org/dav"
username = "owner@example.org"
password_file = "caldav-password"
calendars = ["https://calendar.example.org/home/personal/"]
`), 0600))
	cfg, err := Load(path, "")
	requirements.NoError(err)
	requirements.Len(cfg.CalDAV.Sources, 1)
	source := cfg.GetCalDAVSource("personal")
	requirements.NotNil(source)
	assertions.Equal("UTC", source.TimeZone)
	assertions.Equal(filepath.Join(dir, "caldav-password"), source.PasswordFile)
	assertions.Nil(cfg.GetCalDAVSource("absent"))
}

func TestLoadCalDAVPasswordFileResolvesRelativeToConfigDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	configDir, homeDir := t.TempDir(), t.TempDir()
	configPath := filepath.Join(configDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(`[[caldav.sources]]
name = "personal"
url = "https://calendar.example.org/dav"
username = "owner@example.org"
password_file = "caldav-password"
`), 0600))

	cfg, err := Load(configPath, homeDir)
	require.NoError(err)
	source := cfg.GetCalDAVSource("personal")
	require.NotNil(source)
	assert.Equal(filepath.Join(configDir, "caldav-password"), source.PasswordFile)
}

func TestCalDAVConfigRejectsInvalidSources(t *testing.T) {
	requirements := require.New(t)
	for _, change := range []func(*CalDAVSource){func(s *CalDAVSource) { s.URL = "http://calendar.example.org" }, func(s *CalDAVSource) { s.URL = "https://user:secret@calendar.example.org" }, func(s *CalDAVSource) { s.TimeZone = "Custom/Zone" }, func(s *CalDAVSource) { s.TimeZone = "Local" }, func(s *CalDAVSource) { s.Name = "../other" }, func(s *CalDAVSource) { s.PasswordFile = "" }, func(s *CalDAVSource) { s.Calendars = []string{"https://other.example.org/home/"} }} {
		src := CalDAVSource{Name: "personal", URL: "https://calendar.example.org/dav", Username: "owner@example.org", PasswordFile: "password", TimeZone: "UTC"}
		change(&src)
		cfg := NewDefaultConfig()
		cfg.CalDAV.Sources = []CalDAVSource{src}
		requirements.Error(cfg.validateCalDAV())
	}
	cfg := NewDefaultConfig()
	src := CalDAVSource{Name: "personal", URL: "https://calendar.example.org/dav", Username: "owner@example.org", PasswordFile: "password"}
	cfg.CalDAV.Sources = []CalDAVSource{src, src}
	requirements.ErrorContains(cfg.validateCalDAV(), "duplicate")
}
