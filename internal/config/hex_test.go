package config

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
)

func TestLoadHexSourceDefaultsAndPaths(t *testing.T) {
	path := writeMeetingConfig(t, `[[hex]]
account_email = " You@Example.com "
meetings_dir = "fixtures/meetings"
schedule = "*/30 * * * *"
enabled = true
`)
	cfg, err := Load(path, "")
	require.NoError(t, err)
	require.Len(t, cfg.Hex, 1)
	source := cfg.Hex[0]
	assert.Equal(t, "default", source.Identifier)
	assert.Equal(t, "you@example.com", source.AccountEmail)
	assert.Equal(t, filepath.Join(filepath.Dir(path), "fixtures", "meetings"), source.EffectiveMeetingsDir())
	assert.False(t, source.Recordings)
	assert.Equal(t, int64(4)<<30, source.EffectiveRecordingMaxBytes())
	assert.Len(t, cfg.ScheduledHexSources(), 1)
	require.NotNil(t, cfg.GetHexSource("DEFAULT"))
	assert.Nil(t, cfg.GetHexSource("absent"))
	support := t.TempDir()
	t.Setenv("HEX_APPLICATION_SUPPORT_DIR", support)
	assert.Equal(t, filepath.Join(support, "meetings"), HexSource{}.EffectiveMeetingsDir())
}
func TestLoadHexRejectsInvalidEntries(t *testing.T) {
	for _, body := range []string{
		`[[hex]]`,
		"[[hex]]\naccount_email=\"invalid\"",
		"[[hex]]\nidentifier=\"one\"\naccount_email=\"you@example.com\"\n[[hex]]\nidentifier=\"ONE\"\naccount_email=\"you@example.com\"",
		"[[hex]]\naccount_email=\"you@example.com\"\nrecording_max_bytes=-1",
		"[server]\nhex_recording_max_bytes=-1\n[[hex]]\naccount_email=\"you@example.com\"",
	} {
		_, err := Load(writeMeetingConfig(t, body), "")
		require.Error(t, err)
	}
}

func TestWindowsHexDataDirectoryFallback(t *testing.T) {
	home := t.TempDir()
	assert.Equal(t, filepath.Join(home, "AppData", "Roaming", "hex", "meetings"), windowsHexMeetingsDir(home, ""))
	appData := t.TempDir()
	assert.Equal(t, filepath.Join(appData, "hex", "meetings"), windowsHexMeetingsDir(home, appData))
}
