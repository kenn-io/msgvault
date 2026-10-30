package config

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLineConfigRoundTrip(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	cfg := NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	cfg.Line = []LineSource{
		{ChannelID: "2000000000", SecretEnv: "EXAMPLE_LINE_SECRET_A", Enabled: true},
		{ChannelID: "18446744073709551615", SecretEnv: "EXAMPLE_LINE_SECRET_B", Enabled: false},
	}
	must.NoError(cfg.Save())
	loaded, err := Load(cfg.ConfigFilePath(), "")
	must.NoError(err)
	checks.Equal(cfg.Line, loaded.Line)
	checks.Equal(&cfg.Line[0], loaded.GetLineSource("2000000000"))
	checks.Nil(loaded.GetLineSource("unknown"))
	raw, err := os.ReadFile(cfg.ConfigFilePath())
	must.NoError(err)
	checks.Contains(string(raw), "EXAMPLE_LINE_SECRET_A")
}

func TestLineConfigValidation(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	for _, id := range []string{"", "0", "00", "01", "+1", "-1", "1.0", "1e3", "١", "18446744073709551616", "2000/evil"} {
		t.Run("channel_"+id, func(t *testing.T) {
			err := loadConfigTextError(t, fmt.Sprintf("[[line]]\nchannel_id=%q\nsecret_env=\"EXAMPLE_LINE_SECRET\"\nenabled=false\n", id))
			require.ErrorContains(t, err, "channel_id")
		})
	}
	for _, env := range []string{"", "1KEY", "BAD-NAME", "KEY=value"} {
		t.Run("env_"+env, func(t *testing.T) {
			err := loadConfigTextError(t, fmt.Sprintf("[[line]]\nchannel_id=\"2000000000\"\nsecret_env=%q\n", env))
			require.ErrorContains(t, err, "secret_env")
		})
	}
	cfg := loadConfigText(t, "[[line]]\nchannel_id=\" 2000000000 \"\nsecret_env=\" EXAMPLE_LINE_SECRET \"\nenabled=true\n")
	checks.Equal("2000000000", cfg.Line[0].ChannelID)
	checks.Equal("EXAMPLE_LINE_SECRET", cfg.Line[0].SecretEnv)
	must.ErrorContains(loadConfigTextError(t, `[[line]]
channel_id="2000000000"
secret_env="EXAMPLE_LINE_SECRET"
[[line]]
channel_id=" 2000000000 "
secret_env="EXAMPLE_LINE_SECRET_B"
`), "duplicate")
	for _, extra := range []string{`secret="never-store"`, `schedule="* * * * *"`, `media=true`, `secret_en="EXAMPLE_LINE_SECRET"`} {
		must.ErrorContains(loadConfigTextError(t, "[[line]]\nchannel_id=\"2000000000\"\nsecret_env=\"EXAMPLE_LINE_SECRET\"\n"+extra), "unknown LINE config key")
	}
}
