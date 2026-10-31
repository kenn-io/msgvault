package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalDAVConfigRejectsInvalidSources(t *testing.T) {
	requirements := require.New(t)
	for _, change := range []func(*CalDAVSource){func(s *CalDAVSource) { s.MaxEvents = -1 }, func(s *CalDAVSource) { s.MaxEvents = 200001 }, func(s *CalDAVSource) { s.URL = "http://calendar.example.org" }, func(s *CalDAVSource) { s.URL = "https://user:secret@calendar.example.org" }, func(s *CalDAVSource) { s.TimeZone = "Custom/Zone" }, func(s *CalDAVSource) { s.TimeZone = "Local" }, func(s *CalDAVSource) { s.Name = "../other" }, func(s *CalDAVSource) { s.PasswordFile = "" }, func(s *CalDAVSource) { s.Calendars = []string{"https://other.example.org/home/"} }} {
		src := CalDAVSource{Name: "personal", URL: "https://calendar.example.org/dav", Username: "owner@example.org", PasswordFile: "password", TimeZone: "UTC"}
		cfg := NewDefaultConfig()
		cfg.CalDAV.Sources = []CalDAVSource{src}
		requirements.NoError(cfg.validateCalDAV())
		change(&cfg.CalDAV.Sources[0])
		requirements.Error(cfg.validateCalDAV())
	}
	cfg := NewDefaultConfig()
	src := CalDAVSource{Name: "personal", URL: "https://calendar.example.org/dav", Username: "owner@example.org", PasswordFile: "password"}
	cfg.CalDAV.Sources = []CalDAVSource{src, src}
	requirements.ErrorContains(cfg.validateCalDAV(), "duplicate")
}

func TestCalDAVLoadSavePreservesRelativePasswordAndOmittedDefaults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(`[[caldav.sources]]
name = "personal"
url = "https://calendar.example.org/dav"
username = "owner@example.org"
password_file = "caldav-password"
max_events = 0
`), 0600))
	cfg, err := Load(path, t.TempDir())
	require.NoError(err)
	require.Len(cfg.CalDAV.Sources, 1)
	assert.Nil(cfg.GetCalDAVSource("absent"))
	require.NoError(cfg.Save())
	var saved struct {
		CalDAV struct {
			Sources []map[string]any `toml:"sources"`
		} `toml:"caldav"`
	}
	_, err = toml.DecodeFile(path, &saved)
	require.NoError(err)
	require.Len(saved.CalDAV.Sources, 1)
	source := saved.CalDAV.Sources[0]
	assert.Equal("caldav-password", source["password_file"])
	assert.NotContains(source, "max_events")
	assert.NotContains(source, "time_zone")
	resolved := cfg.GetCalDAVSource("personal")
	require.NotNil(resolved)
	assert.Equal(filepath.Join(filepath.Dir(path), "caldav-password"), resolved.PasswordFile)
	assert.Equal("UTC", resolved.TimeZone)
	assert.Equal(20000, resolved.MaxEvents)
}
