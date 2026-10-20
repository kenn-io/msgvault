package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func runExportMessagesForConfig(t *testing.T, home string) (stdout, stderr string, err error) {
	t.Helper()
	st := testutil.NewTestStore(t)
	root := newRootCommand()
	root.AddCommand(newExportMessagesLocalCmd(exportMessagesDeps{
		openStore: func(context.Context) (*store.Store, func(), error) { return st, func() {}, nil },
	}))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{
		"--home", home, "export-messages",
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z",
	})
	err = root.Execute()
	if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
		inv.logResult.Close()
	}
	return out.String(), errOut.String(), err
}

func TestDaemonOnlyConfigErrorsWarnClientAndStopServe(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	for _, tc := range []struct{ name, content, env, mention string }{
		{name: "kata", content: "[integrations.kata]\nendpoint = '://invalid'\n", mention: "[integrations.kata]"},
		{name: "docbank", content: "[integrations.docbank]\nattachment_folder = '../escape'\n", mention: "docbank"},
		{name: "api port env", env: "invalid", mention: "MSGVAULT_API_PORT"},
		{name: "fastmail", content: "[[fastmail]]\naccount = 'me@example.com'\n", mention: "[[fastmail]]"},
		{name: "oauth client", content: "[oauth]\nclient_secrets_command = []\n", mention: "client_secrets_command"},
		{name: "people sweep", content: "[people.sweep]\nwork_batch_size = -1\n", mention: "work_batch_size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.content), 0o600))
			if tc.env != "" {
				t.Setenv("MSGVAULT_API_PORT", tc.env)
			}

			stdout, stderr, err := runExportMessagesForConfig(t, home)
			require.NoError(err)
			assert.Contains(stdout, `"record_type":"complete"`)
			assert.NotContains(stdout, "Warning")
			assert.Equal(1, strings.Count(stderr, "Warning: ignoring invalid daemon config:"), stderr)
			assert.Contains(stderr, tc.mention)

			t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			_, stderr, err = runExportMessagesForConfig(t, home)
			require.NoError(err)
			assert.Empty(stderr)
			t.Setenv(daemonCLISubprocessEnv, "")

			server := newRootCommand()
			server.AddCommand(&cobra.Command{Use: "serve", RunE: runServe})
			server.SetErr(&bytes.Buffer{})
			server.SetArgs([]string{"--home", home, "serve"})
			err = server.Execute()
			if inv := invocationFromCommand(server); inv != nil && inv.logResult != nil {
				inv.logResult.Close()
			}
			require.Error(err)
			assert.Contains(err.Error(), "daemon config invalid")
			assert.Contains(err.Error(), tc.mention)
		})
	}
}

func TestClientCommandsStillFailOnInvalidClientConfig(t *testing.T) { //nolint:paralleltest // root startup changes the process logger
	for _, content := range []string{"[server\n", "[chatwoot]\nunknown = true\n", "[backup]\nzstd_level = 20\n"} {
		t.Run(content, func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0o600))
			_, _, err := runExportMessagesForConfig(t, home)
			require.ErrorContains(t, err, "load config")
		})
	}
}

func TestStartServeBackgroundProcessRejectsInvalidDaemonConfigBeforeSpawning(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(os.WriteFile(path, []byte("[integrations.kata]\nendpoint = '://invalid'\n"), 0o600))
	cfg, err := config.LoadClientWithOverrides(path, home, config.RuntimeOverrides{})
	require.NoError(err)

	_, err = startServeBackgroundProcess(cfg, backgroundServeStartOptions{
		ExecutablePath: filepath.Join(home, "missing-msgvault"),
	})
	require.ErrorContains(err, "daemon config invalid")
	require.ErrorContains(err, "[integrations.kata]")
	assert.NoFileExists(filepath.Join(cfg.Data.DataDir, "serve.log"))
}
