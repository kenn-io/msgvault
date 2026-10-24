package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
)

func TestListLiveDaemonRuntimeRecordsKeepsForeignRecord(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	// Use a real host identity with a synthetic PID that collides with the
	// helper's PID. The foreign namespace makes its local PID irrelevant.
	identity, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(ok, "read host process identity")
	path, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:               2,
		ProcessIdentityV2: identity,
		Network:           daemon.NetworkTCP,
		Address:           "127.0.0.1:1",
		Service:           daemonService,
	})
	require.NoError(err)
	before, err := os.ReadFile(path)
	require.NoError(err)
	host := daemon.NewRuntimeRecord(daemonService, "v-test", daemon.Endpoint{Network: daemon.NetworkTCP, Address: "127.0.0.1:1"})
	hostPath, err := daemonRuntimeStore(dataDir).Write(host)
	require.NoError(err)
	hostBefore, err := os.ReadFile(hostPath)
	require.NoError(err)

	runDaemonPIDNamespace(t, dataDir, "list")

	after, err := os.ReadFile(path)
	require.NoError(err, "listing preserves the host record on disk")
	assert.Equal(t, before, after, "listing leaves the host record unchanged")
	hostAfter, err := os.ReadFile(hostPath)
	require.NoError(err, "listing preserves the host record whose PID is absent locally")
	assert.Equal(t, hostBefore, hostAfter)
}

func TestClaimServeOwnershipStartsOverDeadSiblingRecord(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	// The first sandbox exits without Close, leaving its runtime record and
	// releasing its kernel locks as a crashed daemon would.
	runDaemonPIDNamespace(t, dataDir, "crash")
	store := daemonRuntimeStore(dataDir)
	path, err := store.Path(2)
	require.NoError(err)
	sibling, err := store.Read(path)
	require.NoError(err, "dead sibling left its record")
	require.NotEmpty(sibling.ProcessIdentityV2)

	runDaemonPIDNamespace(t, dataDir, "restart")

	assert.NoFileExists(path, "normal shutdown removes the replacement record")
	held, err := daemonOwnerLockHeld(dataDir)
	require.NoError(err)
	assert.False(held, "normal shutdown releases ownership")
}

func runDaemonPIDNamespace(t *testing.T, dataDir, mode string) {
	t.Helper()
	bwrap, err := exec.LookPath("bwrap")
	if errors.Is(err, exec.ErrNotFound) {
		t.Skip("Bubblewrap is required for PID namespace tests")
	}
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bwrap, //nolint:gosec // Bubblewrap comes from LookPath; the test binary and flags are controlled here.
		"--unshare-user", "--unshare-pid", "--die-with-parent",
		"--ro-bind", "/", "/", "--bind", dataDir, dataDir, "--proc", "/proc",
		"--", os.Args[0], "-test.run=^TestDaemonRuntimePIDNamespaceHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "MSGVAULT_HOME="+dataDir, "MSGVAULT_TEST_PIDNS="+mode)
	output, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(output), "bwrap:") && (strings.Contains(string(output), "Operation not permitted") || strings.Contains(string(output), "Permission denied")) {
		t.Skipf("user/PID namespaces unavailable: %s", output)
	}
	require.NoError(t, err, "namespace helper (%s):\n%s", mode, output)
}

func TestDaemonRuntimePIDNamespaceHelper(t *testing.T) {
	mode := os.Getenv("MSGVAULT_TEST_PIDNS")
	if mode == "" {
		t.Skip("only runs inside the PID namespace helper")
	}
	require := require.New(t)
	assert := assert.New(t)
	require.Equal(2, os.Getpid(), "helper runs as namespace PID 2 behind Bubblewrap's init")
	dataDir := os.Getenv("MSGVAULT_HOME")
	store := daemonRuntimeStore(dataDir)
	path, err := store.Path(os.Getpid())
	require.NoError(err)
	var previous daemon.RuntimeRecord
	if mode != "crash" {
		previous, err = store.Read(path)
		require.NoError(err)
		require.Equal(daemon.ProcessScopeForeign, daemon.RuntimeProcessScope(previous), "exercise Foreign, rather than Unknown or Gone")
		require.True(daemon.ProcessAlive(previous.PID), "foreign PID collides with a live local process")
	}
	if mode == "list" {
		records, err := store.List()
		require.NoError(err)
		require.Len(records, 2)
		for _, rec := range records {
			require.Equal(daemon.ProcessScopeForeign, daemon.RuntimeProcessScope(rec))
			if rec.PID != os.Getpid() {
				require.False(daemon.ProcessAlive(rec.PID), "host producer's PID is absent inside the sandbox")
			}
		}
		records, err = listLiveDaemonRuntimeRecords(dataDir)
		require.NoError(err)
		assert.Empty(records, "listing skips both foreign records")
		return
	}
	if mode == "crash" {
		// A daemon still publishes a record when process creation time is unavailable.
		stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return 0, false })
	} else {
		starting, err := daemonStartInProgress(t.Context(), dataDir)
		require.NoError(err)
		require.False(starting, "a dead sibling must not block automatic daemon startup")
	}
	cfg := &config.Config{Data: config.DataConfig{DataDir: dataDir}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	owner, err := claimServeOwnership(ctx, cfg, "127.0.0.1", 8123, "v-test")
	require.NoError(err, "serve starts over the dead sibling record")
	if mode == "restart" {
		t.Cleanup(func() { require.NoError(owner.Close()) })
	}
	written, err := store.Read(path)
	require.NoError(err)
	assert.Equal(daemon.ProcessScopeLocal, daemon.RuntimeProcessScope(written))
	assert.NotEmpty(written.Metadata[runtimeShutdownToken], "serve publishes its shutdown credential")
	if mode == "restart" {
		assert.NotEqual(previous.ProcessIdentityV2, written.ProcessIdentityV2, "serve replaces the sibling at the same path")
		assert.NotEqual(previous.Metadata[runtimeShutdownToken], written.Metadata[runtimeShutdownToken])
	}
}
