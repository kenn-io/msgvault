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
	requirements := require.New(t)
	assertions := assert.New(t)
	root := &cobra.Command{Use: "msgvault"}
	source := &cobra.Command{Use: "source"}
	root.AddCommand(source)
	source.AddCommand(newTikTokCommentsCmd())
	for _, action := range []string{"add", "sync"} {
		cmd, args, err := root.Find([]string{"source", "tiktok-comments", action, "example"})
		requirements.NoError(err)
		assertions.Equal(action, cmd.Name())
		assertions.Equal([]string{"example"}, args)
		if action == "sync" {
			requirements.NoError(cmd.Flags().Set("max-requests", "9"))
		}
		forwarded, err := daemonCLIArgsFromCobra(cmd, args)
		requirements.NoError(err)
		assertions.Equal([]string{"source", "tiktok-comments", action}, forwarded[:3])
	}
}

func TestTikTokJSONError(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	cmd := newTikTokCommentsAction("sync")
	out, stderr := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(stderr)
	requirements.NoError(cmd.Flags().Set("json", "true"))
	requirements.NoError(cmd.Flags().Set("max-requests", "0"))
	requirements.Error(cmd.RunE(cmd, nil))
	assertions.Empty(out.String())
	assertions.JSONEq(`{"error":"--max-requests must be positive\nhint: use at least 2 to fetch videos"}`, stderr.String())
}

func TestTikTokEnabledSelection(t *testing.T) {
	requirements := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.TikTok = []config.TikTokSource{{Identifier: "enabled", Enabled: true}, {Identifier: "disabled"}}
	profiles, err := selectedTikTokProfiles(cfg, nil)
	requirements.NoError(err)
	requirements.Len(profiles, 1)
	assert.Equal(t, "enabled", profiles[0].Identifier)
	_, err = selectedTikTokProfiles(cfg, []string{"disabled"})
	requirements.ErrorContains(err, "disabled")
}
