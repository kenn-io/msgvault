package cmd

import (
	"context"
	"errors"
	"net"
	"net/http"
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
	"go.kenn.io/msgvault/internal/daemonauth"
)

func TestWriteDaemonRuntimePublishesKitRecord(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()

	written, shutdownToken, err := writeDaemonRuntime(dataDir, "127.0.0.1", 8123, "v-test", "test-api-key")
	require.NoError(err, "writeDaemonRuntime")
	require.NotEmpty(shutdownToken, "shutdown token")
	t.Cleanup(func() { removeDaemonRuntime(dataDir) })

	path, err := daemon.RuntimeStore{Dir: dataDir}.Path(written.PID)
	require.NoError(err, "runtime record path")
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
	assert.Equal(daemonStartupPhaseInitial, rec.Metadata[runtimeStartupPhase], "startup phase metadata")
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

	written, _, err := writeDaemonRuntime(linkedDataDir, "127.0.0.1", 8123, "v-test", "")
	require.NoError(err, "writeDaemonRuntime through symlink")
	t.Cleanup(func() { removeDaemonRuntime(linkedDataDir) })

	path, err := daemonRuntimeStore(linkedDataDir).Path(written.PID)
	require.NoError(err, "runtime record path through symlink")
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
			runtimeHost:             host,
			runtimePort:             portText,
			runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
			runtimeAPISchemaVersion: api.APISchemaVersion,
			runtimeCreateTime:       matchingProcessCreateTime(t),
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
			runtimeHost:             host,
			runtimePort:             portText,
			runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
			runtimeAPISchemaVersion: api.APISchemaVersion,
		},
	})
	require.NoError(err, "write runtime record")

	assert.Nil(findDaemonRuntime(dataDir), "wrong service ping must not match")
}

func TestFindDaemonRuntimeRejectsUnauthenticatedPingWithoutExactCreateTime(t *testing.T) {
	tests := []struct {
		name     string
		live     int64
		liveOK   bool
		recorded string
	}{
		{name: "unknown create time", liveOK: false, recorded: "1234567890123"},
		{name: "tolerance-only skew", live: 5_000, liveOK: true, recorded: "6000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dataDir := t.TempDir()
			stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return tt.live, tt.liveOK })

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == daemon.DefaultPingPath {
					daemon.NewPingHandler(daemon.PingHandlerOptions{
						Service: daemonService,
						Version: "v-test",
					}).ServeHTTP(w, r)
					return
				}
				http.NotFound(w, r)
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
					runtimeHost:             host,
					runtimePort:             portText,
					runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
					runtimeAPISchemaVersion: api.APISchemaVersion,
					runtimeCreateTime:       tt.recorded,
					runtimeShutdownToken:    "private-runtime-secret",
				},
			})
			require.NoError(err, "write runtime record")

			rt, found, err := findRespondingDaemonRuntime(context.Background(), dataDir,
				func(*DaemonRuntime, error) bool { return true })

			require.NoError(err, "find responding runtime")
			assert.False(found, "an unauthenticated ping cannot prove process identity")
			assert.Nil(rt, "unproven endpoint must not become discoverable")
		})
	}
}

func TestFindDaemonRuntimeAcceptsRuntimeSecretProofWhenCreateTimeUnknown(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return 0, false })
	const runtimeSecret = "private-runtime-secret"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case api.DaemonIdentityPath:
			proof, err := daemonauth.Proof(runtimeSecret,
				r.Header.Get(api.DaemonIdentityChallengeHeader), os.Getpid())
			if err != nil {
				http.Error(w, "invalid challenge", http.StatusBadRequest)
				return
			}
			w.Header().Set(api.DaemonIdentityProofHeader, proof)
			w.WriteHeader(http.StatusNoContent)
		case daemon.DefaultPingPath:
			daemon.NewPingHandler(daemon.PingHandlerOptions{
				Service: daemonService,
				Version: "v-test",
			}).ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
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
			runtimeHost:             host,
			runtimePort:             portText,
			runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
			runtimeAPISchemaVersion: api.APISchemaVersion,
			runtimeCreateTime:       "1234567890123",
			runtimeShutdownToken:    runtimeSecret,
		},
	})
	require.NoError(err, "write runtime record")

	rt := findDaemonRuntime(dataDir)

	require.NotNil(rt, "runtime secret proof recovers an indeterminate identity")
	assert.Equal(os.Getpid(), rt.Record.PID, "pid")
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

func stubProcessCreateTimeMillis(t *testing.T, fn func(int) (int64, bool)) {
	t.Helper()
	prev := processCreateTimeMillisForRun
	processCreateTimeMillisForRun = fn
	t.Cleanup(func() { processCreateTimeMillisForRun = prev })
}

func matchingProcessCreateTime(t *testing.T) string {
	t.Helper()
	created, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok, "read current process create time")
	return strconv.FormatInt(created, 10)
}

func assertRuntimeRecordFileExists(t *testing.T, dataDir string) {
	t.Helper()
	path, err := daemonRuntimeStore(dataDir).Path(os.Getpid())
	require.NoError(t, err, "runtime record path")
	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "runtime record file must not be deleted")
}

func TestListLiveDaemonRuntimeRecordsKeepsRecordWithSkewedCreateTime(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()

	live, ok := processCreateTimeMillis(os.Getpid())
	require.True(ok, "read live create time")

	// One second of skew is what whole-second boot-time jitter produces in
	// containerized deployments; it must not disqualify a live daemon.
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(live+1000, 10),
		},
	})
	require.NoError(err, "write runtime record")

	records, err := listLiveDaemonRuntimeRecords(dataDir)

	require.NoError(err, "list live records")
	require.Len(records, 1, "skewed-but-live record stays discoverable")
	assert.Equal(os.Getpid(), records[0].PID, "pid")
	assertRuntimeRecordFileExists(t, dataDir)
}

func TestCompareProcessCreateTime(t *testing.T) {
	const base = int64(1_000_000_000_000)

	tests := []struct {
		name     string
		recorded string
		live     int64
		liveOK   bool
		want     createTimeComparison
	}{
		{name: "exact match", recorded: "1000000000000", live: base, liveOK: true, want: createTimeMatch},
		{name: "skew below tolerance", recorded: "999999999000", live: base, liveOK: true, want: createTimeSkew},
		{name: "skew at tolerance boundary", recorded: "1000000002000", live: base, liveOK: true, want: createTimeSkew},
		{name: "skew beyond tolerance", recorded: "999999997999", live: base, liveOK: true, want: createTimeMismatch},
		{name: "unparseable recorded value", recorded: "not-a-number", live: base, liveOK: true, want: createTimeUnknown},
		{name: "gopsutil failure", recorded: "1000000000000", live: 0, liveOK: false, want: createTimeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return tt.live, tt.liveOK })

			got := compareProcessCreateTime(os.Getpid(), tt.recorded)

			assert.Equal(t, tt.want, got, "comparison outcome")
		})
	}
}

func TestProcessCreateTimeMatchesRequiresAffirmativeMatch(t *testing.T) {
	assert := assert.New(t)
	stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return 5_000, true })

	assert.True(processCreateTimeMatches(os.Getpid(), "5000"), "exact create time matches")
	assert.False(processCreateTimeMatches(os.Getpid(), "6000"), "tolerance-only skew is not authoritative")
	assert.False(processCreateTimeMatches(os.Getpid(), "10000"), "beyond tolerance does not match")
	assert.False(processCreateTimeMatches(os.Getpid(), "bogus"), "indeterminate comparison does not match")
}

func TestListLiveDaemonRuntimeRecordsKeepsRecordWhenCreateTimeUnknown(t *testing.T) {
	writeRecord := func(t *testing.T, dataDir, createTime string) {
		t.Helper()
		_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
			PID:     os.Getpid(),
			Network: daemon.NetworkTCP,
			Address: "127.0.0.1:1",
			Service: daemonService,
			Metadata: map[string]string{
				runtimeCreateTime: createTime,
			},
		})
		require.NoError(t, err, "write runtime record")
	}

	t.Run("gopsutil failure", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		dataDir := t.TempDir()
		stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return 0, false })
		writeRecord(t, dataDir, "1234567890123")

		records, err := listLiveDaemonRuntimeRecords(dataDir)

		require.NoError(err, "list live records")
		require.Len(records, 1, "indeterminate create time keeps the record live")
		assert.Equal(os.Getpid(), records[0].PID, "pid")
		assertRuntimeRecordFileExists(t, dataDir)
	})

	t.Run("unparseable create_time", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		dataDir := t.TempDir()
		writeRecord(t, dataDir, "not-a-number")

		records, err := listLiveDaemonRuntimeRecords(dataDir)

		require.NoError(err, "list live records")
		require.Len(records, 1, "unparseable create time keeps the record live")
		assert.Equal(os.Getpid(), records[0].PID, "pid")
		assertRuntimeRecordFileExists(t, dataDir)
	})
}

func TestFindDaemonRuntimeRejectsRespondingEndpointWithMismatchedCreateTime(t *testing.T) {
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

	live, ok := processCreateTimeMillis(os.Getpid())
	require.True(ok, "read live create time")

	// A responding endpoint is not process-identity proof. Once the local
	// create time confirms PID reuse, an unauthenticated ping must not make
	// the stale record discoverable.
	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: "v-test",
		Metadata: map[string]string{
			runtimeHost:             host,
			runtimePort:             portText,
			runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
			runtimeAPISchemaVersion: api.APISchemaVersion,
			runtimeCreateTime:       strconv.FormatInt(live+10*60*1000, 10),
		},
	})
	require.NoError(err, "write runtime record")

	rt := findDaemonRuntime(dataDir)

	assert.Nil(rt, "mismatched process identity must outweigh an unauthenticated ping")
	assertRuntimeRecordFileExists(t, dataDir)
}

func TestListLiveDaemonRuntimeRecordsSkipsUnverifiableMismatchWithoutDeleting(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()

	live, ok := processCreateTimeMillis(os.Getpid())
	require.True(ok, "read live create time")

	// Beyond-tolerance mismatch and nothing answering on the recorded
	// address: the record must not count as live, but the file stays on
	// disk for inspection (kit's CleanupDead reaps it once the PID dies).
	_, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(live+10*60*1000, 10),
		},
	})
	require.NoError(err, "write runtime record")

	records, err := listLiveDaemonRuntimeRecords(dataDir)

	require.NoError(err, "list live records")
	assert.Empty(records, "unverifiable identity mismatch must not count as live")
	assertRuntimeRecordFileExists(t, dataDir)
}
