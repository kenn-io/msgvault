package cmd

import (
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
)

func TestWriteDaemonRuntimePublishesKitRecord(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()

	path, shutdownToken, err := writeDaemonRuntime(dataDir, "127.0.0.1", 8123, "v-test", "test-api-key")
	require.NoError(err, "writeDaemonRuntime")
	require.NotEmpty(shutdownToken, "shutdown token")
	t.Cleanup(func() { removeDaemonRuntime(dataDir) })

	rec, err := daemon.RuntimeStore{Dir: dataDir}.Read(path)
	require.NoError(err, "read runtime record")

	assert.Equal(daemonService, rec.Service, "service")
	assert.Equal("v-test", rec.Version, "version")
	assert.Equal(daemon.NetworkTCP, rec.Network, "network")
	assert.Equal(net.JoinHostPort("127.0.0.1", "8123"), rec.Address, "address")
	assert.Equal("127.0.0.1", rec.Metadata[runtimeHost], "host metadata")
	assert.Equal(strconv.Itoa(8123), rec.Metadata[runtimePort], "port metadata")
	assert.Equal(strconv.Itoa(daemonAPIVersion), rec.Metadata[runtimeAPIVersion], "api version metadata")
	assert.Equal(api.APISchemaVersion, rec.Metadata[runtimeAPISchemaVersion], "api schema metadata")
	assert.Equal(daemonAPIKeyFingerprint("test-api-key"), rec.Metadata[runtimeAuthFingerprint], "api key fingerprint metadata")
	assert.Equal(shutdownToken, rec.Metadata[runtimeShutdownToken], "shutdown token metadata")
}

func TestWriteDaemonRuntimeAcceptsSymlinkedDataDir(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	parentDir := t.TempDir()
	realDataDir := filepath.Join(parentDir, "real")
	linkedDataDir := filepath.Join(parentDir, "linked")
	require.NoError(os.Mkdir(realDataDir, 0o700), "create real data directory")
	if err := os.Symlink(realDataDir, linkedDataDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	path, _, err := writeDaemonRuntime(linkedDataDir, "127.0.0.1", 8123, "v-test", "")
	require.NoError(err, "writeDaemonRuntime through symlink")
	t.Cleanup(func() { removeDaemonRuntime(linkedDataDir) })

	rec, err := daemonRuntimeStore(linkedDataDir).Read(path)
	require.NoError(err, "read runtime record through symlink")
	assert.Equal(os.Getpid(), rec.PID, "pid")
	assert.Equal(daemonService, rec.Service, "service")
}

func TestFindDaemonRuntimeRequiresLiveMsgvaultPing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "v-test",
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(err, "split listener address")
	port, err := strconv.Atoi(portText)
	require.NoError(err, "parse listener port")

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: "v-test",
		Metadata: map[string]string{
			runtimeHost:       host,
			runtimePort:       portText,
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(err, "write runtime record")

	rt := findDaemonRuntime(dataDir)
	require.NotNil(rt, "runtime should be discovered")
	assert.Equal(os.Getpid(), rt.Record.PID, "pid")
	assert.Equal(host, rt.Host, "host")
	assert.Equal(port, rt.Port, "port")
}

func TestFindDaemonRuntimeRejectsWrongServicePing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: "other",
		Version: "v-test",
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(err, "split listener address")

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: "v-test",
		Metadata: map[string]string{
			runtimeHost:       host,
			runtimePort:       portText,
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion),
		},
	})
	require.NoError(err, "write runtime record")

	assert.Nil(findDaemonRuntime(dataDir), "wrong service ping must not match")
}

func TestListLiveDaemonRuntimeRecordsFiltersServiceAndDeadProcesses(t *testing.T) {
	t.Run("wrong service", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		dataDir := t.TempDir()
		_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
			PID:     os.Getpid(),
			Network: daemon.NetworkTCP,
			Address: "127.0.0.1:1",
			Service: "other",
		})
		require.NoError(err, "write wrong-service runtime")

		records, err := listLiveDaemonRuntimeRecords(dataDir)

		require.NoError(err, "list live records")
		assert.Empty(records, "wrong-service record")
	})

	t.Run("dead process", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		dataDir := t.TempDir()
		_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
			PID:     2147483647,
			Network: daemon.NetworkTCP,
			Address: "127.0.0.1:1",
			Service: daemonService,
		})
		require.NoError(err, "write dead-process runtime")

		records, err := listLiveDaemonRuntimeRecords(dataDir)

		require.NoError(err, "list live records")
		assert.Empty(records, "dead-process record")
	})

	require := require.New(t)
	assert := assert.New(t)

	dataDir := t.TempDir()
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
	})
	require.NoError(err, "write live runtime")

	records, err := listLiveDaemonRuntimeRecords(dataDir)

	require.NoError(err, "list live records")
	require.Len(records, 1, "records")
	assert.Equal(os.Getpid(), records[0].PID, "pid")
	assert.Equal(daemonService, records[0].Service, "service")
}

func TestShouldUpgradeDaemonRuntimePolicy(t *testing.T) {
	tests := []struct {
		name           string
		daemonVersion  string
		currentVersion string
		want           bool
	}{
		{
			name:           "newer release replaces older daemon",
			daemonVersion:  "v1.0.0",
			currentVersion: "v1.1.0",
			want:           true,
		},
		{
			name:           "same release does not restart",
			daemonVersion:  "v1.0.0",
			currentVersion: "v1.0.0",
			want:           false,
		},
		{
			name:           "older release does not downgrade newer daemon",
			daemonVersion:  "v1.1.0",
			currentVersion: "v1.0.0",
			want:           false,
		},
		{
			name:           "release treats missing daemon version as old",
			daemonVersion:  "",
			currentVersion: "v1.0.0",
			want:           true,
		},
		{
			name:           "dev does not replace missing daemon version",
			daemonVersion:  "",
			currentVersion: "dev",
			want:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			rt := &DaemonRuntime{Record: daemon.RuntimeRecord{Version: tt.daemonVersion}}
			assert.Equal(tt.want, shouldUpgradeDaemonRuntimeWithPolicy(rt, tt.currentVersion, config.DaemonAutoRestartNewer), "upgrade decision")
		})
	}
}

func TestShouldUpgradeDaemonRuntimeWithConfiguredPolicy(t *testing.T) {
	tests := []struct {
		name           string
		policy         string
		daemonVersion  string
		currentVersion string
		want           bool
	}{
		{
			name:           "newer policy restarts older daemon",
			policy:         config.DaemonAutoRestartNewer,
			daemonVersion:  "v1.0.0",
			currentVersion: "v1.1.0",
			want:           true,
		},
		{
			name:           "newer policy does not downgrade newer daemon",
			policy:         config.DaemonAutoRestartNewer,
			daemonVersion:  "v1.1.0",
			currentVersion: "v1.0.0",
			want:           false,
		},
		{
			name:           "never policy leaves older compatible daemon alone",
			policy:         config.DaemonAutoRestartNever,
			daemonVersion:  "v1.0.0",
			currentVersion: "v1.1.0",
			want:           false,
		},
		{
			name:           "always policy replaces newer daemon when explicitly requested",
			policy:         config.DaemonAutoRestartAlways,
			daemonVersion:  "v1.1.0",
			currentVersion: "v1.0.0",
			want:           true,
		},
		{
			name:           "always policy keeps same version",
			policy:         config.DaemonAutoRestartAlways,
			daemonVersion:  "v1.0.0",
			currentVersion: "v1.0.0",
			want:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			rt := &DaemonRuntime{Record: daemon.RuntimeRecord{Version: tt.daemonVersion}}
			assert.Equal(tt.want,
				shouldUpgradeDaemonRuntimeWithPolicy(rt, tt.currentVersion, tt.policy),
				"upgrade decision")
		})
	}
}

func TestIncompatibleDaemonMessageUsesCallerGuidance(t *testing.T) {
	err := incompatibleDaemonError(
		errors.New("daemon API version 1 is incompatible with client API version 2"),
		"run `msgvault daemon stop` or retry with --local",
	)

	require.Error(t, err, "incompatible daemon error")
	assert.Contains(t, err.Error(), "incompatible daemon is already running")
	assert.Contains(t, err.Error(), "run `msgvault daemon stop` or retry with --local")
}
