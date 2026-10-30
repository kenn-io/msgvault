package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/update"
)

func TestPerformUpdateWithDaemonLifecycleRestartsStoppedDaemon(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	var events []string

	err := performUpdateWithDaemonLifecycle(
		&update.UpdateInfo{LatestVersion: "v0.17.0"},
		nil,
		func() (*config.Config, error) {
			events = append(events, "load")
			return cfg, nil
		},
		func(got *config.Config) (updateDaemonStopResult, error) {
			events = append(events, "stop")
			assert.Same(cfg, got, "stop config")
			return updateDaemonStopResult{Stopped: true}, nil
		},
		func(info *update.UpdateInfo, _ func(int64, int64)) error {
			events = append(events, "perform")
			assert.Equal("v0.17.0", info.LatestVersion, "latest version")
			return nil
		},
		func(got *config.Config, result updateDaemonStopResult) error {
			events = append(events, "restart")
			assert.Same(cfg, got, "restart config")
			assert.True(result.Stopped, "stopped")
			return nil
		},
	)

	require.NoError(err, "perform update")
	assert.Equal([]string{"load", "stop", "perform", "restart"}, events)
}

func TestPerformUpdateWithDaemonLifecycleDoesNotRestartWhenNoneStopped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	var events []string

	err := performUpdateWithDaemonLifecycle(
		&update.UpdateInfo{LatestVersion: "v0.17.0"},
		nil,
		func() (*config.Config, error) {
			events = append(events, "load")
			return cfg, nil
		},
		func(*config.Config) (updateDaemonStopResult, error) {
			events = append(events, "stop")
			return updateDaemonStopResult{}, nil
		},
		func(*update.UpdateInfo, func(int64, int64)) error {
			events = append(events, "perform")
			return nil
		},
		func(*config.Config, updateDaemonStopResult) error {
			events = append(events, "restart")
			return nil
		},
	)

	require.NoError(err, "perform update")
	assert.Equal([]string{"load", "stop", "perform"}, events)
}

func TestPerformUpdateWithDaemonLifecycleRestartsAfterInstallFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	installErr := errors.New("install failed")
	var events []string

	err := performUpdateWithDaemonLifecycle(
		&update.UpdateInfo{LatestVersion: "v0.17.0"},
		nil,
		func() (*config.Config, error) {
			events = append(events, "load")
			return cfg, nil
		},
		func(*config.Config) (updateDaemonStopResult, error) {
			events = append(events, "stop")
			return updateDaemonStopResult{Stopped: true}, nil
		},
		func(*update.UpdateInfo, func(int64, int64)) error {
			events = append(events, "perform")
			return installErr
		},
		func(*config.Config, updateDaemonStopResult) error {
			events = append(events, "restart")
			return nil
		},
	)

	require.ErrorIs(err, installErr, "install error")
	assert.Equal([]string{"load", "stop", "perform", "restart"}, events)
}

func TestPerformUpdateWithDaemonLifecycleReportsRestartFailureAfterInstallFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	installErr := errors.New("install failed")
	restartErr := errors.New("restart failed")

	err := performUpdateWithDaemonLifecycle(
		&update.UpdateInfo{LatestVersion: "v0.17.0"},
		nil,
		func() (*config.Config, error) {
			return cfg, nil
		},
		func(*config.Config) (updateDaemonStopResult, error) {
			return updateDaemonStopResult{Stopped: true}, nil
		},
		func(*update.UpdateInfo, func(int64, int64)) error {
			return installErr
		},
		func(*config.Config, updateDaemonStopResult) error {
			return restartErr
		},
	)

	require.ErrorIs(err, installErr, "install error")
	require.ErrorIs(err, restartErr, "restart error")
	assert.Contains(err.Error(), "also failed to restart daemon")
}

func TestPerformUpdateWithDaemonLifecycleRestartsAfterPartialStopFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	stopErr := errors.New("stop failed")
	var events []string

	err := performUpdateWithDaemonLifecycle(
		&update.UpdateInfo{LatestVersion: "v0.17.0"},
		nil,
		func() (*config.Config, error) {
			events = append(events, "load")
			return cfg, nil
		},
		func(*config.Config) (updateDaemonStopResult, error) {
			events = append(events, "stop")
			return updateDaemonStopResult{Stopped: true}, stopErr
		},
		func(*update.UpdateInfo, func(int64, int64)) error {
			events = append(events, "perform")
			return nil
		},
		func(*config.Config, updateDaemonStopResult) error {
			events = append(events, "restart")
			return nil
		},
	)

	require.ErrorIs(err, stopErr, "stop error")
	assert.Equal([]string{"load", "stop", "restart"}, events)
}

func TestStopLocalDaemonsForUpdateStopsLiveRuntimeRecords(t *testing.T) {
	for _, background := range []string{"", "false", "true"} {
		t.Run("background="+background, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dataDir := t.TempDir()
			server := httptestPingDaemon(t)
			portText := strconv.Itoa(server.Port)
			record := daemon.RuntimeRecord{
				PID:     os.Getpid(),
				Network: daemon.NetworkTCP,
				Address: net.JoinHostPort(server.Host, portText),
				Service: daemonService,
				Version: Version,
				Metadata: map[string]string{
					runtimeHost:             server.Host,
					runtimePort:             portText,
					runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
					runtimeAPISchemaVersion: api.APISchemaVersion,
					runtimeCreateTime:       matchingProcessCreateTime(t),
				},
			}
			if background != "" {
				record.Metadata[runtimeBackground] = background
			}
			_, err := daemonRuntimeStore(dataDir).Write(record)
			require.NoError(err, "write runtime")

			var stoppedPID int
			stubStopDaemonRuntimeForUpgrade(t, func(_ config.Config, rt *DaemonRuntime, _ *slog.Logger) error {
				stoppedPID = rt.Record.PID
				return nil
			})

			result, err := stopLocalDaemonsForUpdate(lifecycleTestConfig(dataDir), testDiscardLogger())

			require.NoError(err, "stop local daemons")
			assert.True(result.Stopped, "stopped")
			assert.Equal(background == "true", result.Background, "background")
			assert.Equal(os.Getpid(), stoppedPID, "stopped pid")
		})
	}
}

func TestRestartDaemonAfterUpdateUsesInstalledExecutablePath(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	installedExe := filepath.Join(t.TempDir(), "bin", updateExecutableName())
	waitCh := make(chan error)
	var gotExecutable string
	var gotCtx context.Context

	stubStartServeBackgroundProcess(t, func(
		_ *config.Config,
		opts backgroundServeStartOptions,
	) (*backgroundServeProcess, error) {
		gotExecutable = opts.ExecutablePath
		return &backgroundServeProcess{
			PID:     990,
			LogPath: "/tmp/msgvault-serve.log",
			Wait:    waitCh,
		}, nil
	})
	stubWaitForBackgroundServeReady(t, func(
		ctx context.Context,
		_ string,
		_ <-chan error,
		_ time.Duration,
	) (*DaemonRuntime, bool, error) {
		gotCtx = ctx
		return &DaemonRuntime{
			Record: daemon.RuntimeRecord{PID: 990},
			Host:   net.IPv4(127, 0, 0, 1).String(),
			Port:   9090,
		}, true, nil
	})

	err := restartDaemonAfterUpdate(cfg, updateDaemonStopResult{Stopped: true, Background: true}, installedExe, testInvocationWithConfig(cfg), false, io.Discard)

	require.NoError(err, "restart daemon")
	assert.Equal(installedExe, gotExecutable, "restart executable")
	require.NotNil(gotCtx, "readiness wait received a nil context")
	assert.NoError(gotCtx.Err(), "context usable")
}

func TestRestartDaemonAfterUpdateLeavesUnmarkedDaemonStopped(t *testing.T) {
	assert := assert.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		assert.Fail("unmarked daemon must be left to whoever started it")
		return nil, errors.New("unexpected daemon start")
	})
	for _, noRestart := range []bool{false, true} {
		state := testInvocationWithConfig(cfg)
		var out bytes.Buffer
		err := restartDaemonAfterUpdate(cfg, updateDaemonStopResult{Stopped: true}, "", state, noRestart, &out)
		require.NoError(t, err)
		assert.Equal("Update couldn't confirm msgvault started the daemon for "+cfg.Data.DataDir+" in the background, so it left it stopped. Restart it the way you started it, such as `systemctl restart msgvault`.\n", out.String())
		assert.NotContains(out.String(), "daemon start")
	}
}

func TestRestartDaemonAfterUpdateNoRestart(t *testing.T) {
	assert := assert.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
		assert.Fail("--no-restart must leave the daemon stopped")
		return nil, errors.New("unexpected daemon start")
	})
	for _, tt := range []struct {
		options invocationOptions
		command string
	}{
		{command: "    msgvault daemon start\n"},
		{
			options: invocationOptions{homeDir: "/tmp/example archive", cfgFile: "/tmp/example config.toml"},
			command: "    msgvault --config '/tmp/example config.toml' --home '/tmp/example archive' daemon start\n",
		},
	} {
		state := testInvocationWithConfig(cfg)
		state.options = tt.options
		var out bytes.Buffer
		err := restartDaemonAfterUpdate(cfg, updateDaemonStopResult{Stopped: true, Background: true}, "", state, true, &out)
		require.NoError(t, err)
		assert.Contains(out.String(), cfg.Data.DataDir)
		assert.Contains(out.String(), tt.command)
	}
}

// fakeUpdateRelease stands in for GitHub: checks report a newer release, and
// installs are recorded instead of replacing the test binary.
func fakeUpdateRelease(t *testing.T) (checks, installs *int) {
	t.Helper()
	checks, installs = new(int), new(int)
	savedCheck, savedPerform := checkForUpdate, performUpdate
	t.Cleanup(func() { checkForUpdate, performUpdate = savedCheck, savedPerform })
	checkForUpdate = func(current string, _ bool) (*update.UpdateInfo, error) {
		*checks++
		return &update.UpdateInfo{CurrentVersion: current, LatestVersion: "v9.9.9", Checksum: "abc"}, nil
	}
	performUpdate = func(info *update.UpdateInfo, _ func(int64, int64)) error {
		*installs++
		assert.Equal(t, "v9.9.9", info.LatestVersion, "installed version")
		return nil
	}
	return checks, installs
}

func runUpdateWithDistribution(t *testing.T, distribution string, args ...string) error {
	t.Helper()
	saved := Distribution
	Distribution = distribution
	t.Cleanup(func() { Distribution = saved })
	// Keep the command away from the user's archive and update cache.
	home := t.TempDir()
	t.Setenv("MSGVAULT_HOME", home)
	root := newRootCommand()
	root.AddCommand(newUpdateCmd())
	_, _, err := runRoot(t, root, append([]string{"--home", home, "update"}, args...)...)
	return err
}

func TestUpdateRefusesToInstallOverHomebrewBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	for _, args := range [][]string{nil, {"--yes"}, {"--yes", "--force"}} {
		checks, installs := fakeUpdateRelease(t)

		err := runUpdateWithDistribution(t, "homebrew", args...)

		require.ErrorContains(err, "managed by Homebrew", "msgvault update %v", args)
		require.ErrorContains(err, "`brew upgrade msgvault`", "msgvault update %v", args)
		assert.Zero(*checks, "msgvault update %v checked GitHub", args)
		assert.Zero(*installs, "msgvault update %v installed", args)
	}
}

func TestUpdateInstallsOverSelfUpdatingBuilds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	for _, distribution := range []string{"", "kenn"} {
		checks, installs := fakeUpdateRelease(t)

		err := runUpdateWithDistribution(t, distribution, "--yes")

		require.NoError(err, "distribution %q", distribution)
		assert.Equal(1, *checks, "distribution %q checks", distribution)
		assert.Equal(1, *installs, "distribution %q installs", distribution)
	}
}
