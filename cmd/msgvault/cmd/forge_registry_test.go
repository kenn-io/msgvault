package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
)

func TestForgeRegistryConnectsCommandsConfigAndTokenEnvironment(t *testing.T) {
	cfg := &config.Config{
		GitHub: config.ForgeConfig{Enabled: true, Schedule: "0 * * * *", IncludeOwners: []string{"github-owner"}},
		GitLab: config.ForgeConfig{Schedule: "30 * * * *", ExcludeOwners: []string{"gitlab-owner"}},
		Gitea:  config.ForgeConfig{Enabled: true, IncludeOwners: []string{"gitea-owner"}},
	}
	for _, tc := range []struct {
		name string
		want config.ForgeConfig
	}{
		{name: "github", want: cfg.GitHub},
		{name: "gitlab", want: cfg.GitLab},
		{name: "gitea", want: cfg.Gitea},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			fs, ok := forgeFor(tc.name)
			require.True(ok)
			require.NotNil(fs)
			assert.Equal(tc.name, fs.name)
			assert.True(clirun.EnvAllowed(fs.tokenEnv), "the token must reach the daemon subprocess")
			assert.Equal(tc.want, fs.config(cfg))
			for _, prefix := range []string{"add-", "sync-"} {
				name := prefix + tc.name
				command, remaining, err := rootCmd.Find([]string{name})
				require.NoError(err)
				require.NotNil(command)
				assert.Equal(name, command.Name())
				assert.Empty(remaining)
				assert.Same(rootCmd, command.Parent())
			}
		})
	}
}
