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
	_, err := configuredYouTubeClient(config.YouTubeSource{Identifier: "fixture", ClientIDEnv: "FIXTURE_YT_CLIENT", ClientSecretEnv: "FIXTURE_YT_SECRET", TokenEnv: "FIXTURE_YT_REFRESH"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "fixture-value")
}
