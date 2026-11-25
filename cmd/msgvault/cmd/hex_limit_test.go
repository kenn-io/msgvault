package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestSyncHexRejectsNegativeLimitBeforeWork(t *testing.T) {
	old := syncHexLimit
	syncHexLimit = -1
	t.Cleanup(func() { syncHexLimit = old })
	for _, url := range []string{"", "http://127.0.0.1:1"} {
		t.Run(map[bool]string{true: "remote", false: "local"}[url != ""], func(t *testing.T) {
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			cfg.Remote.URL = url
			command := commandWithInvocation(t, &cobra.Command{}, cfg, invocationOptions{})
			require.ErrorContains(t, syncHexCmd.RunE(command, nil), "--limit must be nonnegative")
		})
	}
}
