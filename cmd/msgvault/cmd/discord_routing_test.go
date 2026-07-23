package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscordCommandsRouteThroughDaemonCLIRunner(t *testing.T) {
	tests := []struct {
		name     string
		wantArgs []string
		command  func(discordCommandDeps) *cobra.Command
		args     []string
		stdin    string
		wantEnv  map[string]string
	}{
		{
			name: "add Discord",
			wantArgs: []string{
				"add-discord", "--guild=113456789012345678", "--oauth-app=archive",
			},
			command: newAddDiscordCmd,
			args:    []string{"--guild", "113456789012345678", "--oauth-app", "archive"},
			stdin:   "synthetic-discord-token\n",
			wantEnv: map[string]string{"MSGVAULT_DISCORD_TOKEN": "synthetic-discord-token"},
		},
		{
			name:     "sync Discord",
			wantArgs: []string{"sync-discord", "--after=2026-01-01", "--full", "113456789012345678"},
			command:  newSyncDiscordCmd,
			args:     []string{"113456789012345678", "--full", "--after", "2026-01-01"},
		},
		{
			name:     "backfill Discord media",
			wantArgs: []string{"backfill-discord-media", "--only-incomplete", "113456789012345678"},
			command:  newBackfillDiscordMediaCmd,
			args:     []string{"113456789012345678", "--only-incomplete"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				assert.Equal(t, tt.wantArgs, req.Args)
				assert.Equal(t, tt.wantEnv, req.Env)
			}, `{"type":"complete"}`)
			configureRemoteDaemonForTest(t, server.URL)
			t.Setenv(daemonCLISubprocessEnv, "")

			cmd := tt.command(discordCommandDeps{})
			cmd.SetIn(strings.NewReader(tt.stdin))
			cmd.SetArgs(tt.args)
			require.NoError(t, cmd.Execute())
			assert.Equal(t, 1, int(requests.Load()))
		})
	}
}
