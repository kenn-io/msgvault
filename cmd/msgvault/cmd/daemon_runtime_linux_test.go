package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"testing/synctest"
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

func TestDaemonAutoStartOverDeadSiblingRecord(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	// The first sandbox exits without Close, leaving its runtime record and
	// releasing its kernel locks as a crashed daemon would.
	runDaemonPIDNamespace(t, dataDir, "crash")
	store := daemonRuntimeStore(dataDir)
	path, err := store.Path(2)
	require.NoError(err)
	_, err = store.Read(path)
	require.NoError(err, "dead sibling left its record")

	runDaemonPIDNamespace(t, dataDir, "restart")

	assert.NoFileExists(path, "normal shutdown removes the replacement record")
}

var daemonPIDNamespaceProbe struct {
	sync.Once

	output []byte
	err    error
}

func runDaemonPIDNamespace(t *testing.T, dataDir, mode string) {
	t.Helper()
	bwrap, err := exec.LookPath("bwrap")
	if errors.Is(err, exec.ErrNotFound) {
		if os.Getenv("CI") != "" {
			require.NoError(t, err, "Bubblewrap is required for PID namespace tests in CI")
		}
		t.Skip("Bubblewrap is required for PID namespace tests")
	}
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	args := []string{
		"--unshare-user", "--unshare-pid", "--die-with-parent",
		"--ro-bind", "/", "/", "--bind", dataDir, dataDir, "--proc", "/proc",
		// TestMain creates its isolated home inside the writable fixture.
		"--setenv", "TMPDIR", dataDir, "--",
	}
	daemonPIDNamespaceProbe.Do(func() {
		probe := exec.CommandContext(ctx, bwrap, append(args, os.Args[0], "-test.run=^$")...) //nolint:gosec // Bubblewrap comes from LookPath; the test binary and flags are controlled here.
		daemonPIDNamespaceProbe.output, daemonPIDNamespaceProbe.err = probe.CombinedOutput()
	})
	if daemonPIDNamespaceProbe.err != nil {
		if os.Getenv("CI") != "" {
			require.NoError(t, daemonPIDNamespaceProbe.err, "user/PID namespaces unavailable in CI:\n%s", daemonPIDNamespaceProbe.output)
		}
		t.Skipf("user/PID namespaces unavailable: %v\n%s", daemonPIDNamespaceProbe.err, daemonPIDNamespaceProbe.output)
	}
	helperArgs := append([]string{"--info-fd", "3", "--block-fd", "4"}, args...)
	cmd := exec.CommandContext(ctx, bwrap, append(helperArgs, os.Args[0], "-test.run=^TestDaemonRuntimePIDNamespaceHelper$", "-test.v")...) //nolint:gosec // Bubblewrap comes from LookPath; the test binary and flags are controlled here.
	cmd.Env = append(os.Environ(), "MSGVAULT_TEST_PIDNS_DATA_DIR="+dataDir, "MSGVAULT_TEST_PIDNS="+mode)
	infoRead, infoWrite, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = infoRead.Close() }()
	defer func() { _ = infoWrite.Close() }()
	blockRead, blockWrite, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = blockRead.Close() }()
	defer func() { _ = blockWrite.Close() }()
	cmd.ExtraFiles = []*os.File{infoWrite, blockRead}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Start())
	defer func() {
		if cmd.ProcessState == nil {
			cancel()
			_ = cmd.Wait()
		}
	}()
	require.NoError(t, infoWrite.Close())
	require.NoError(t, blockRead.Close())
	var info struct {
		ChildPID int `json:"child-pid"`
	}
	require.NoError(t, json.NewDecoder(infoRead).Decode(&info))
	// Keep the namespace inode allocated after the helper exits. Otherwise
	// Linux can reuse it for the sibling, making the stale record look local.
	namespace, err := os.Open(fmt.Sprintf("/proc/%d/ns/pid", info.ChildPID))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, namespace.Close()) })
	_, err = blockWrite.Write([]byte{1})
	require.NoError(t, err)
	err = cmd.Wait()
	require.NoError(t, err, "namespace helper (%s):\n%s", mode, &output)
}

func TestDaemonRuntimePIDNamespaceHelper(t *testing.T) {
	mode := os.Getenv("MSGVAULT_TEST_PIDNS")
	if mode == "" {
		t.Skip("only runs inside the PID namespace helper")
	}
	requirements := require.New(t)
	assert := assert.New(t)
	requirements.Equal(2, os.Getpid(), "helper runs as namespace PID 2 behind Bubblewrap's init")
	dataDir := os.Getenv("MSGVAULT_TEST_PIDNS_DATA_DIR")
	store := daemonRuntimeStore(dataDir)
	path, err := store.Path(os.Getpid())
	requirements.NoError(err)
	if mode != "crash" {
		previous, err := store.Read(path)
		requirements.NoError(err)
		requirements.Equal(daemon.ProcessScopeForeign, daemon.RuntimeProcessScope(previous), "exercise Foreign, rather than Unknown or Gone")
		requirements.True(daemon.ProcessAlive(previous.PID), "foreign PID collides with a live local process")
	}
	if mode == "list" {
		records, err := store.List()
		requirements.NoError(err)
		for _, rec := range records {
			if rec.PID != os.Getpid() {
				requirements.Equal(daemon.ProcessScopeForeign, daemon.RuntimeProcessScope(rec))
				requirements.False(daemon.ProcessAlive(rec.PID), "host producer's PID is absent inside the sandbox")
			}
		}
		records, err = listLiveDaemonRuntimeRecords(dataDir)
		requirements.NoError(err)
		assert.Empty(records, "listing skips both foreign records")
		return
	}
	if mode == "crash" {
		// Production omits create_time when the process read fails. This keeps
		// a missing scope guard observable even after the create-time tolerance.
		stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return 0, false })
	}
	cfg := &config.Config{Data: config.DataConfig{DataDir: dataDir}}
	if mode == "restart" {
		synctest.Test(t, func(t *testing.T) {
			_, launchLock, err := waitForUsableBackgroundRuntimeOrLaunchLock(t.Context(), dataDir, cfg.Server.DaemonAutoRestart, 500*time.Millisecond)
			require.NoError(t, err)
			require.NotNil(t, launchLock, "a dead sibling must allow CLI auto-start")
			require.NoError(t, launchLock.Unlock())
		})
	}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	owner, err := claimServeOwnership(ctx, cfg, "127.0.0.1", 8123, "v-test")
	requirements.NoError(err, "serve starts over the dead sibling record")
	if mode == "restart" {
		t.Cleanup(func() { requirements.NoError(owner.Close()) })
	}
}
