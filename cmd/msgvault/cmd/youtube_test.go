package cmd

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"testing"
)

func TestConfiguredYouTubeCredentialsMissingWithoutLeak(t *testing.T) {
	t.Setenv("FIXTURE_YT_CLIENT", "fixture-value")
	t.Setenv("FIXTURE_YT_SECRET", "")
	_, err := configuredYouTubeClient(t.Context(), config.YouTubeSource{Identifier: "fixture", ClientIDEnv: "FIXTURE_YT_CLIENT", ClientSecretEnv: "FIXTURE_YT_SECRET", TokenEnv: "FIXTURE_YT_REFRESH"})
	require.ErrorContains(t, err, "FIXTURE_YT_SECRET")
	assert.NotContains(t, err.Error(), "fixture-value")
}

func TestYouTubeNestedDaemonCommands(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	for _, action := range []string{"add", "sync"} {
		leaf, _, err := rootCmd.Find([]string{"source", "youtube", action})
		require.NoError(err)
		if action == "sync" {
			require.NoError(leaf.Flags().Set("max-requests", "7"))
			t.Cleanup(func() {
				_ = leaf.Flags().Set("max-requests", "1000")
				leaf.Flags().Lookup("max-requests").Changed = false
			})
		}
		forwarded, err := daemonCLIArgsFromCobra(leaf, []string{"example"})
		require.NoError(err)
		if action == "sync" {
			assert.Equal([]string{"source", "youtube", "sync", "--max-requests=7", "example"}, forwarded)
		} else {
			assert.Equal([]string{"source", "youtube", "add", "example"}, forwarded)
		}
	}
	assert.True(manualSyncCLICommand([]string{"source", "youtube", "sync", "example"}))
	assert.False(manualSyncCLICommand([]string{"source", "youtube", "add", "example"}))
}
