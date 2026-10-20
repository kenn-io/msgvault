package cmd

import (
	"bytes"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
)

func runExportMessagesForConfig(t *testing.T, home string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCommand()
	root.AddCommand(newExportMessagesCmd(exportMessagesDeps{}))
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

func TestDaemonOnlyConfigErrorsWarnClientAndStopDaemonStart(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	for _, tc := range []struct{ name, content, env, mention string }{
		{name: "kata", content: "[integrations.kata]\nendpoint = '://invalid'\n", mention: "[integrations.kata]"},
		{name: "docbank", content: "[integrations.docbank]\nattachment_folder = '../escape'\n", mention: "docbank"},
		{name: "api port env", env: "invalid", mention: "MSGVAULT_API_PORT"},
		{name: "fastmail", content: "[[fastmail]]\naccount = 'me@example.com'\n", mention: "[[fastmail]]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			if tc.env != "" {
				t.Setenv("MSGVAULT_API_PORT", tc.env)
			}
			t.Setenv(daemonCLISubprocessEnv, "")

			// A reachable daemon runs the export; the client only warns.
			server, requests := newDaemonCLIRunnerTestServer(t, nil,
				"{\"type\":\"stdout\",\"data\":\"{\\\"record_type\\\":\\\"complete\\\"}\\n\"}\n"+
					"{\"type\":\"complete\"}\n")
			home := t.TempDir()
			remote := "\n[remote]\nurl = '" + server.URL + "'\nallow_insecure = true\n"
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.content+remote), 0o600))
			stdout, stderr, err := runExportMessagesForConfig(t, home)
			require.NoError(err)
			assert.Equal(int32(1), requests.Load())
			assert.Contains(stdout, `"record_type":"complete"`)
			assert.NotContains(stdout, "Warning")
			assert.Equal(1, strings.Count(stderr, "Warning: invalid daemon config:"), stderr)
			assert.Contains(stderr, tc.mention)

			// With no daemon, autostart refuses before it spawns one.
			// The real launcher must refuse first; a missing executable keeps a regression harmless.
			stubStartServeBackgroundProcess(t, func(c *config.Config, opts backgroundServeStartOptions) (*backgroundServeProcess, error) {
				opts.ExecutablePath = filepath.Join(c.HomeDir, "missing-msgvault")
				return startServeBackgroundProcess(c, opts)
			})
			home = t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.content), 0o600))
			_, stderr, err = runExportMessagesForConfig(t, home)
			require.ErrorContains(err, "start background daemon: daemon config invalid")
			assert.NoFileExists(filepath.Join(home, "serve.log"))
			assert.Equal(1, strings.Count(stderr, tc.mention), "report the error once: %q", stderr)

			server2 := newRootCommand()
			server2.AddCommand(&cobra.Command{Use: "serve", RunE: runServe})
			var serveErr bytes.Buffer
			server2.SetErr(&serveErr)
			server2.SetArgs([]string{"--home", home, "serve"})
			err = server2.Execute()
			if inv := invocationFromCommand(server2); inv != nil && inv.logResult != nil {
				inv.logResult.Close()
			}
			require.ErrorContains(err, "daemon config invalid")
			assert.Equal(1, strings.Count(serveErr.String(), tc.mention), "report the error once: %q", serveErr.String())
		})
	}
}

func TestClientCommandsStillFailOnInvalidClientConfig(t *testing.T) { //nolint:paralleltest // root startup changes the process logger
	for _, content := range []string{
		"[server\n",
		"[chatwoot]\nunknown = true\n",
		"[backup]\nzstd_level = 20\n",
		"[server]\ndaemon_auto_restart = 'sometimes'\n",
		"[oauth]\nclient_secrets_command = []\n",
		"[people.sweep]\nwork_batch_size = -1\n",
	} {
		t.Run(content, func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0o600))
			_, _, err := runExportMessagesForConfig(t, home)
			require.ErrorContains(t, err, "load config")
		})
	}
}

func TestSemanticPersonVectorConfigSourceToleratesInvalidDaemonConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	configured := config.NewDefaultConfig()
	configured.HomeDir = t.TempDir()
	configured.Data.DataDir = configured.HomeDir
	configured.Vector.Enabled = true
	configured.Vector.Embeddings.Endpoint = "https://embedding.example.test/v1"
	configured.Vector.Embeddings.Model = "synthetic-config-source-model"
	configured.Vector.Embeddings.Dimension = 4
	configured.Vector.People = vector.PeopleConfig{
		Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
	}
	testCtx := withTestConfig(t, configured)
	require.NoError(os.WriteFile(configured.ConfigFilePath(), []byte(`
[vector]
enabled = true
[vector.embeddings]
endpoint = "https://embedding.example.test/v1"
model = "synthetic-config-source-model"
dimension = 4
[vector.people]
enabled = true
retention_posture = "zero_data_retention"
training_posture = "no_training"
[integrations.kata]
endpoint = "://invalid"
`), 0o600))
	current, err := currentSemanticPersonVectorConfigSource(invocationFromContext(testCtx))()
	require.NoError(err)
	assert.True(current.People.Enabled)
}

func TestDaemonStartRejectsInvalidDaemonConfigBeforeReusingDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	home := filepath.Join(t.TempDir(), "home")
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[integrations.kata]\nendpoint = '://invalid'\n"), 0o600))
	loaded, err := config.LoadClientWithOverrides(path, home, config.RuntimeOverrides{})
	require.NoError(err)

	start := newLifecycleCommand("start", false)
	start.SetContext(testInvocationContext(t.Context(), loaded, invocationOptions{homeDir: home}))
	err = start.RunE(start, nil)
	require.ErrorContains(err, "daemon config invalid")
	assert.NoDirExists(home)
}

func TestInvalidDaemonConfigDoesNotStopStaleDaemon(t *testing.T) {
	for _, tc := range []struct {
		name, daemonVersion string
		reuse               bool
	}{
		{"stale daemon", "v1.0.0", false},
		{"compatible daemon", "v1.1.0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			withTestVersion(t, "v1.1.0")
			home := t.TempDir()
			path := filepath.Join(home, "config.toml")
			require.NoError(os.WriteFile(path, []byte("[integrations.kata]\nendpoint = '://invalid'\n"), 0o600))
			cfg, err := config.LoadClientWithOverrides(path, home, config.RuntimeOverrides{})
			require.NoError(err)
			cfg.Server.DaemonAutoRestart = config.DaemonAutoRestartNewer

			// The runtime record points at a helper process so a wrongly
			// issued stop cannot signal the test binary.
			helper := helperProcessCommand(t.Context(), "block")
			require.NoError(helper.Start())
			t.Cleanup(func() { _ = helper.Process.Kill() })
			created, ok := processCreateTimeMillis(helper.Process.Pid)
			require.True(ok)
			ping := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
				Service: daemonService, Version: tc.daemonVersion, PID: helper.Process.Pid,
			}))
			t.Cleanup(ping.Close)
			host, portText, err := net.SplitHostPort(ping.Listener.Addr().String())
			require.NoError(err)
			_, err = daemonRuntimeStore(home).Write(daemon.RuntimeRecord{
				PID:     helper.Process.Pid,
				Network: daemon.NetworkTCP,
				Address: net.JoinHostPort(host, portText),
				Service: daemonService,
				Version: tc.daemonVersion,
				Metadata: map[string]string{
					runtimeHost:             host,
					runtimePort:             portText,
					runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
					runtimeAPISchemaVersion: api.APISchemaVersion,
					runtimeCreateTime:       strconv.FormatInt(created, 10),
				},
			})
			require.NoError(err)

			prep, err := prepareBackgroundDaemonStart(cfg, cfg.Server.DaemonAutoRestart, "", slog.Default())
			if tc.reuse {
				require.NoError(err)
				assert.NotNil(prep.Reusable)
			} else {
				require.ErrorContains(err, "daemon config invalid")
				require.ErrorContains(err, "[integrations.kata]")
			}
			assert.NoError(helper.Process.Signal(syscall.Signal(0)), "daemon must still be running")
		})
	}
}
