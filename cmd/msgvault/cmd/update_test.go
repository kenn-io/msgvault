package cmd

import (
	"errors"
	"net"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
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
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server := httptestPingDaemon(t)
	portText := strconv.Itoa(server.Port)
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(server.Host, portText),
		Service: daemonService,
		Version: Version,
		Metadata: map[string]string{
			runtimeHost:       server.Host,
			runtimePort:       portText,
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(err, "write runtime")

	var stoppedPID int
	stubStopDaemonRuntimeForUpgrade(t, func(_ config.Config, rt *DaemonRuntime) error {
		stoppedPID = rt.Record.PID
		return nil
	})

	result, err := stopLocalDaemonsForUpdate(lifecycleTestConfig(dataDir))

	require.NoError(err, "stop local daemons")
	assert.True(result.Stopped, "stopped")
	assert.Equal(os.Getpid(), stoppedPID, "stopped pid")
}
