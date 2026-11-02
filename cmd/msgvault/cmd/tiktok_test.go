package cmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestTikTokCommandGrammarAndProxy(t *testing.T) {
	root := &cobra.Command{Use: "msgvault"}
	source := &cobra.Command{Use: "source"}
	root.AddCommand(source)
	source.AddCommand(newTikTokCommentsCmd())
	for _, action := range []string{"add", "sync"} {
		cmd, args, err := root.Find([]string{"source", "tiktok-comments", action, "example"})
		require.NoError(t, err)
		assert.Equal(t, action, cmd.Name())
		assert.Equal(t, []string{"example"}, args)
		if action == "sync" {
			require.NoError(t, cmd.Flags().Set("max-requests", "9"))
		}
		forwarded, err := daemonCLIArgsFromCobra(cmd, args)
		require.NoError(t, err)
		assert.Equal(t, []string{"source", "tiktok-comments", action}, forwarded[:3])
	}
}

func TestTikTokJSONError(t *testing.T) {
	cmd := newTikTokCommentsAction("sync")
	out, stderr := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(stderr)
	require.NoError(t, cmd.Flags().Set("json", "true"))
	require.NoError(t, cmd.Flags().Set("max-requests", "0"))
	require.Error(t, cmd.RunE(cmd, nil))
	assert.Empty(t, out.String())
	assert.JSONEq(t, `{"error":"--max-requests must be positive\nhint: use at least 2 to fetch videos"}`, stderr.String())
}

func TestTikTokEnabledSelection(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.TikTok = []config.TikTokSource{{Identifier: "enabled", Enabled: true}, {Identifier: "disabled"}}
	profiles, err := selectedTikTokProfiles(cfg, nil)
	require.NoError(t, err)
	require.Len(t, profiles, 1)
	assert.Equal(t, "enabled", profiles[0].Identifier)
	_, err = selectedTikTokProfiles(cfg, []string{"disabled"})
	require.ErrorContains(t, err, "disabled")
}
