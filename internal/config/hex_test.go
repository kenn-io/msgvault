package config

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
)

func TestLoadHexSourceDefaultsAndPaths(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	path := writeMeetingConfig(t, `[[hex]]
account_email = " You@Example.com "
meetings_dir = "fixtures/meetings"
schedule = "*/30 * * * *"
enabled = true
`)
	cfg, err := Load(path, "")
	must.NoError(err)
	must.Len(cfg.Hex, 1)
	source := cfg.Hex[0]
	check.Equal("default", source.Identifier)
	check.Equal("you@example.com", source.AccountEmail)
	check.Equal(filepath.Join(filepath.Dir(path), "fixtures", "meetings"), source.EffectiveMeetingsDir())
	check.False(source.Recordings)
	check.Equal(int64(4)<<30, source.EffectiveRecordingMaxBytes())
	check.Len(cfg.ScheduledHexSources(), 1)
	must.NotNil(cfg.GetHexSource("DEFAULT"))
	check.Nil(cfg.GetHexSource("absent"))
	support := t.TempDir()
	t.Setenv("HEX_APPLICATION_SUPPORT_DIR", support)
	check.Equal(filepath.Join(support, "meetings"), HexSource{}.EffectiveMeetingsDir())
}
func TestLoadHexRejectsInvalidEntries(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"missing account", `[[hex]]`, `[[hex]] identifier "default" requires account_email`},
		{"invalid account", "[[hex]]\naccount_email=\"invalid\"", `[[hex]] identifier "default" has invalid account_email`},
		{"duplicate identifier", "[[hex]]\nidentifier=\"one\"\naccount_email=\"you@example.com\"\n[[hex]]\nidentifier=\"ONE\"\naccount_email=\"you@example.com\"", `[[hex]]: duplicate identifier "ONE"`},
		{"recorder cap", "[[hex]]\naccount_email=\"you@example.com\"\nrecording_max_bytes=-1", "hex recording_max_bytes must be nonnegative and below MaxInt64"},
		{"server cap", "[server]\nhex_recording_max_bytes=-1\n[[hex]]\naccount_email=\"you@example.com\"", "server.hex_recording_max_bytes must be nonnegative and below MaxInt64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeMeetingConfig(t, tc.body), "")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestWindowsHexDataDirectoryFallback(t *testing.T) {
	home := t.TempDir()
	assert.Equal(t, filepath.Join(home, "AppData", "Roaming", "hex", "meetings"), windowsHexMeetingsDir(home, ""))
	appData := t.TempDir()
	assert.Equal(t, filepath.Join(appData, "hex", "meetings"), windowsHexMeetingsDir(home, appData))
}
