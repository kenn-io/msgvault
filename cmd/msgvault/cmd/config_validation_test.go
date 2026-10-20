package cmd

import (
	"bytes"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
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

const invalidKataConfig = "[integrations.kata]\nendpoint = '://invalid'\n"

func writeInvalidKataHome(t *testing.T, extra string) string {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(invalidKataConfig+extra), 0o600))
	t.Setenv(daemonCLISubprocessEnv, "")
	return home
}

func TestInvalidDaemonConfigWarnsOnceAndExportReachesDaemon(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	assert := assert.New(t)
	require := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t, nil,
		"{\"type\":\"stdout\",\"data\":\"{\\\"record_type\\\":\\\"complete\\\"}\\n\"}\n"+
			"{\"type\":\"complete\"}\n")
	home := writeInvalidKataHome(t, "\n[remote]\nurl = '"+server.URL+"'\nallow_insecure = true\n")

	stdout, stderr, err := runExportMessagesForConfig(t, home)
	require.NoError(err)
	assert.Equal(int32(1), requests.Load())
	assert.Contains(stdout, `"record_type":"complete"`)
	assert.NotContains(stdout, "Warning")
	assert.Equal(1, strings.Count(stderr, "Warning: invalid daemon config:"), stderr)
	assert.Contains(stderr, "[integrations.kata]")
}

func TestInvalidDaemonConfigRefusesAutostart(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	assert := assert.New(t)
	home := writeInvalidKataHome(t, "")
	// The real launcher must refuse first; a missing executable keeps a regression harmless.
	stubStartServeBackgroundProcess(t, func(c *config.Config, opts backgroundServeStartOptions) (*backgroundServeProcess, error) {
		opts.ExecutablePath = filepath.Join(c.HomeDir, "missing-msgvault")
		return startServeBackgroundProcess(c, opts)
	})

	_, stderr, err := runExportMessagesForConfig(t, home)
	require.ErrorContains(t, err, "start background daemon: daemon config invalid")
	assert.NoFileExists(filepath.Join(home, "serve.log"))
	assert.Equal(1, strings.Count(stderr, "[integrations.kata]"), "report the error once: %q", stderr)
}

func TestInvalidDaemonConfigRefusesServe(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	home := writeInvalidKataHome(t, "")
	root := newRootCommand()
	root.AddCommand(&cobra.Command{Use: "serve", RunE: runServe})
	var errOut bytes.Buffer
	root.SetErr(&errOut)
	root.SetArgs([]string{"--home", home, "serve"})
	err := root.Execute()
	if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
		inv.logResult.Close()
	}
	require.ErrorContains(t, err, "daemon config invalid")
	assert.Equal(t, 1, strings.Count(errOut.String(), "[integrations.kata]"), "report the error once: %q", errOut.String())
}

func TestClientCommandsFailOnInvalidClientConfig(t *testing.T) { //nolint:paralleltest // root startup changes the process logger
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[backup]\nzstd_level = 20\n"), 0o600))
	_, _, err := runExportMessagesForConfig(t, home)
	require.ErrorContains(t, err, "load config")
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

// writeRunningDaemon records a running daemon of the given version in home. It
// points at a helper process so a wrongly issued stop cannot signal the test binary.
func writeRunningDaemon(t *testing.T, home, version string) *exec.Cmd {
	t.Helper()
	require := require.New(t)
	helper := helperProcessCommand(t.Context(), "block")
	require.NoError(helper.Start())
	t.Cleanup(func() { _ = helper.Process.Kill() })
	created, ok := processCreateTimeMillis(helper.Process.Pid)
	require.True(ok)
	ping := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService, Version: version, PID: helper.Process.Pid,
	}))
	t.Cleanup(ping.Close)
	host, portText, err := net.SplitHostPort(ping.Listener.Addr().String())
	require.NoError(err)
	_, err = daemonRuntimeStore(home).Write(daemon.RuntimeRecord{
		PID:     helper.Process.Pid,
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: version,
		Metadata: map[string]string{
			runtimeHost:             host,
			runtimePort:             portText,
			runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
			runtimeAPISchemaVersion: api.APISchemaVersion,
			runtimeCreateTime:       strconv.FormatInt(created, 10),
		},
	})
	require.NoError(err)
	return helper
}

func loadInvalidKataConfig(t *testing.T, home string) *config.Config {
	t.Helper()
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(invalidKataConfig), 0o600))
	cfg, err := config.LoadClientWithOverrides(path, home, config.RuntimeOverrides{})
	require.NoError(t, err)
	return cfg
}

func TestDaemonStartRejectsInvalidDaemonConfigInsteadOfReusingDaemon(t *testing.T) {
	require := require.New(t)
	withTestVersion(t, "v1.1.0")
	home := t.TempDir()
	writeRunningDaemon(t, home, "v1.1.0")
	cfg := loadInvalidKataConfig(t, home)

	start := newLifecycleCommand("start", false)
	var stdout bytes.Buffer
	start.SetOut(&stdout)
	start.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{homeDir: home}))
	require.ErrorContains(start.RunE(start, nil), "daemon config invalid")
	require.NotContains(stdout.String(), "already running")
}

func TestInvalidDaemonConfigLeavesRunningDaemonAlone(t *testing.T) {
	for _, tc := range []struct {
		name, daemonVersion string
		reuse               bool
	}{
		{"stale daemon is not stopped", "v1.0.0", false},
		{"compatible daemon is reused", "v1.1.0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			withTestVersion(t, "v1.1.0")
			home := t.TempDir()
			helper := writeRunningDaemon(t, home, tc.daemonVersion)
			cfg := loadInvalidKataConfig(t, home)
			cfg.Server.DaemonAutoRestart = config.DaemonAutoRestartNewer

			prep, err := prepareBackgroundDaemonStart(cfg, cfg.Server.DaemonAutoRestart, "", slog.Default())
			if tc.reuse {
				require.NoError(err)
				require.NotNil(prep.Reusable)
				return
			}
			require.ErrorContains(err, "daemon config invalid")
			require.ErrorContains(err, "[integrations.kata]")
			require.NoError(helper.Process.Signal(syscall.Signal(0)), "daemon must still be running")
		})
	}
}
