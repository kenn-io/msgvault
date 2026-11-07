package config

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLineConfigValidation(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	t.Run("maximum_channel_id", func(t *testing.T) {
		require.NoError(t, loadConfigTextError(t, `[[line]]
channel_id="18446744073709551615"
secret_env="EXAMPLE_LINE_SECRET"
enabled=false
`))
	})
	for _, id := range []string{"", "0", "00", "01", "+1", "-1", "1.0", "1e3", "١", "18446744073709551616", "2000/evil", " 2000000001 "} {
		t.Run("channel_"+id, func(t *testing.T) {
			err := loadConfigTextError(t, fmt.Sprintf("[[line]]\nchannel_id=%q\nsecret_env=\"EXAMPLE_LINE_SECRET\"\nenabled=false\n", id))
			require.ErrorContains(t, err, "line[0].channel_id: 1–18446744073709551615, no leading zeros")
		})
	}
	for _, env := range []string{"", "1KEY", "BAD-NAME", "KEY=value"} {
		t.Run("env_"+env, func(t *testing.T) {
			err := loadConfigTextError(t, fmt.Sprintf("[[line]]\nchannel_id=\"2000000000\"\nsecret_env=%q\n", env))
			require.ErrorContains(t, err, "secret_env")
		})
	}
	cfg := loadConfigText(t, "[[line]]\nchannel_id=\"2000000000\"\nsecret_env=\" EXAMPLE_LINE_SECRET \"\nenabled=true\n")
	checks.Equal("2000000000", cfg.Line[0].ChannelID)
	checks.Equal("EXAMPLE_LINE_SECRET", cfg.Line[0].SecretEnv)
	must.ErrorContains(loadConfigTextError(t, `[[line]]
channel_id="2000000000"
secret_env="EXAMPLE_LINE_SECRET"
[[line]]
channel_id="2000000000"
secret_env="EXAMPLE_LINE_SECRET_B"
`), "duplicate")
	for _, limit := range []string{"-1", "8796093022208"} {
		require.ErrorContains(t, loadConfigTextError(t, "[[line]]\nchannel_id=\"2000000000\"\nsecret_env=\"EXAMPLE_LINE_SECRET\"\nmax_media_mb="+limit), "max_media_mb")
	}
	for _, extra := range []string{`secret="never-store"`, `schedule="* * * * *"`, `secret_en="EXAMPLE_LINE_SECRET"`} {
		must.ErrorContains(loadConfigTextError(t, "[[line]]\nchannel_id=\"2000000000\"\nsecret_env=\"EXAMPLE_LINE_SECRET\"\n"+extra), "unknown LINE config key")
	}
}

func TestLineSendConfig(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        bool
	}{
		{"default", "", false}, {"enabled", "allow_send=true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := loadConfigText(t, "[[line]]\nchannel_id=\"2000000001\"\nsecret_env=\"EXAMPLE_LINE_SECRET\"\nenabled=true\n"+tc.field+"\n")
			assert.Equal(tc.want, cfg.Line[0].AllowSend)
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = t.TempDir()
			require.NoError(cfg.Save())
			reloaded, err := Load(cfg.ConfigFilePath(), "")
			require.NoError(err)
			assert.Equal(tc.want, reloaded.Line[0].AllowSend)
		})
	}
}
