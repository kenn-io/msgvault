package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
)

const invalidKataConfig = "[integrations.kata]\nendpoint = '://invalid'\n"

// runRoot executes args on root the way main does, so the post-run warning prints.
func runRoot(t *testing.T, root *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = executeRootContext(t.Context(), root)
	return out.String(), errOut.String(), err
}

func exportMessagesArgs(home string) []string {
	return []string{
		"--home", home, "export-messages",
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z",
	}
}

func newExportRoot() *cobra.Command {
	root := newRootCommand()
	root.AddCommand(newExportMessagesCmd(exportMessagesDeps{}))
	return root
}

// writeInvalidKataHome returns a home whose config has a bad kata endpoint.
func writeInvalidKataHome(t *testing.T, extra string) string {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(invalidKataConfig+extra), 0o600))
	return home
}

// serveRunningDaemon records a running daemon of version in home, answering
// pings as the stand-in process and CLI runs with the given events. The stop
// seam fails the test if anything tries to stop it.
func serveRunningDaemon(t *testing.T, home, version string, apiVersion int, events ...string) (requests *atomic.Int32) {
	t.Helper()
	require := require.New(t)
	standIn := startBlockingDaemonStandIn(t)
	pid := standIn.Process.Pid
	created, ok := processCreateTimeMillis(pid)
	require.True(ok)
	requests = &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService, Version: version, PID: pid,
	}))
	mux.HandleFunc("/api/v1/cli/run", daemonCLIRunTestHandler(t, requests, nil, events...))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	rt := daemonRuntimeForHTTPServer(t, server, daemonAPIKeyFingerprint(""))
	rt.Record.PID = pid
	rt.Record.Version = version
	rt.Record.Metadata[runtimeAPIVersion] = strconv.Itoa(apiVersion)
	rt.Record.Metadata[runtimeCreateTime] = strconv.FormatInt(created, 10)
	_, err := daemonRuntimeStore(home).Write(rt.Record)
	require.NoError(err)
	stubStopDaemonRuntimeForUpgrade(t, func(config.Config, *DaemonRuntime, *slog.Logger) error {
		require.FailNow("a daemon must not be stopped while the config is invalid")
		return errors.New("unreachable")
	})
	return requests
}

func TestInvalidDaemonConfigWarnsOnceAndExportReachesDaemon(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	assert := assert.New(t)
	require := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t, nil, exportCompleteEvents)
	home := writeInvalidKataHome(t, "\n[remote]\nurl = '"+server.URL+"'\nallow_insecure = true\n")

	stdout, stderr, err := runRoot(t, newExportRoot(), exportMessagesArgs(home)...)
	require.NoError(err)
	assert.Equal(int32(1), requests.Load())
	assert.Contains(stdout, `"record_type":"complete"`)
	assert.NotContains(stdout, "Warning")
	assert.Equal(1, strings.Count(stderr, "Warning: invalid daemon config:"), stderr)
	assert.Contains(stderr, "[integrations.kata]")
}

func TestInvalidDaemonConfigReusesOlderCompatibleDaemon(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	assert := assert.New(t)
	require := require.New(t)
	withTestVersion(t, "v1.1.0")
	home := writeInvalidKataHome(t, "")
	requests := serveRunningDaemon(t, home, "v1.0.0", daemonAPIVersion, exportCompleteEvents)
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		require.FailNow("a running daemon must be reused")
		return nil, errors.New("unreachable")
	})

	stdout, stderr, err := runRoot(t, newExportRoot(), exportMessagesArgs(home)...)
	require.NoError(err, stderr)
	assert.Equal(int32(1), requests.Load())
	assert.Contains(stdout, `"record_type":"complete"`)
}

func TestInvalidDaemonConfigIsNotRepeatedAfterProxiedChildFailure(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	assert := assert.New(t)
	childStderr, err := json.Marshal(api.CLIRunEvent{
		Type: "stderr", Data: "Error: load config: invalid [integrations.kata] endpoint \"://invalid\"\n",
	})
	require.NoError(t, err)
	childExit, err := json.Marshal(api.CLIRunEvent{Type: "error", Error: cliSubprocessExitSentinel})
	require.NoError(t, err)
	server, _ := newDaemonCLIRunnerTestServer(t, nil, string(childStderr), string(childExit))
	home := writeInvalidKataHome(t, "\n[remote]\nurl = '"+server.URL+"'\nallow_insecure = true\n")

	_, stderr, err := runRoot(t, newExportRoot(), exportMessagesArgs(home)...)
	require.ErrorIs(t, err, errCLISubprocessProxied)
	assert.Equal(1, strings.Count(stderr, "[integrations.kata]"), "the child already reported it: %q", stderr)
}

func TestInvalidDaemonConfigKeepsIncompatibleGuidanceWhenAutostartIsOff(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	withTestVersion(t, "v1.1.0")
	home := writeInvalidKataHome(t, "[server]\ndaemon_auto_start = false\n")
	serveRunningDaemon(t, home, "v1.0.0", daemonAPIVersion+1)

	_, _, err := runRoot(t, newExportRoot(), exportMessagesArgs(home)...)
	require.ErrorContains(t, err, "incompatible daemon is already running")
	require.NotContains(t, err.Error(), "daemon config invalid")
}

func TestInvalidDaemonConfigRefusesAutostartOnce(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	for _, tc := range []struct {
		name       string
		incompatOK bool
	}{
		{name: "no daemon"},
		{name: "incompatible daemon", incompatOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			withTestVersion(t, "v1.1.0")
			home := writeInvalidKataHome(t, "")
			if tc.incompatOK {
				serveRunningDaemon(t, home, "v1.0.0", daemonAPIVersion+1)
			}
			stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
				require.FailNow(t, "autostart must not launch a daemon from an invalid config")
				return nil, errors.New("unreachable")
			})

			_, stderr, err := runRoot(t, newExportRoot(), exportMessagesArgs(home)...)
			require.ErrorContains(t, err, "start background daemon: daemon config invalid")
			assert.Equal(1, strings.Count(stderr, "[integrations.kata]"), "report the error once: %q", stderr)
		})
	}
}

func TestInvalidDaemonConfigFailsStrictCommandsAtLoad(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	for _, args := range [][]string{
		{"serve"},
		{"daemon", "start"},
		{"daemon", "restart"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home := writeInvalidKataHome(t, "")
			root := newRootCommand()
			root.AddCommand(newDaemonCommand())
			root.AddCommand(&cobra.Command{Use: "serve", RunE: runServe, Annotations: serveCmd.Annotations})
			_, _, err := runRoot(t, root, append([]string{"--home", home}, args...)...)
			require.ErrorContains(t, err, "load config")
		})
	}
}

func TestDaemonCLIChildStaysQuietAboutInvalidDaemonConfig(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	home := writeInvalidKataHome(t, "")
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	root := newRootCommand()
	root.AddCommand(&cobra.Command{Use: "noop", Run: func(*cobra.Command, []string) {}})

	_, stderr, err := runRoot(t, root, "--home", home, "noop")
	require.NoError(t, err)
	assert.Empty(t, stderr)
}

func TestInvalidDaemonConfigIsReportedOnceByStrictSetupCommand(t *testing.T) { //nolint:paralleltest // root startup changes the process logger and environment
	home := writeInvalidKataHome(t, "")
	setup := &cobra.Command{Use: "setup"}
	setup.AddCommand(newSetupProvidersCommand(defaultSetupProvidersDeps(t.Context())))
	root := newRootCommand()
	root.AddCommand(setup)

	_, stderr, err := runRoot(t, root, "--home", home, "setup", "providers", "--dry-run")
	require.ErrorContains(t, err, "[integrations.kata]")
	require.Equal(t, 1, strings.Count(stderr, "[integrations.kata]"), "report the error once: %q", stderr)
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
