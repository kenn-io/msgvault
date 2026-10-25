package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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
	collidingPath, err := daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:               2,
		ProcessIdentityV2: identity,
		Network:           daemon.NetworkTCP,
		Address:           "127.0.0.1:1",
		Service:           daemonService,
	})
	require.NoError(err)
	collidingBefore, err := os.ReadFile(collidingPath)
	require.NoError(err)
	host := daemon.NewRuntimeRecord(daemonService, "v-test", daemon.Endpoint{Network: daemon.NetworkTCP, Address: "127.0.0.1:1"})
	hostPath, err := daemonRuntimeStore(dataDir).Write(host)
	require.NoError(err)
	hostBefore, err := os.ReadFile(hostPath)
	require.NoError(err)

	runDaemonPIDNamespace(t, dataDir, "list")

	collidingAfter, err := os.ReadFile(collidingPath)
	require.NoError(err, "listing preserves the foreign record whose PID collides locally")
	assert.Equal(t, collidingBefore, collidingAfter, "listing leaves the colliding foreign record unchanged")
	hostAfter, err := os.ReadFile(hostPath)
	require.NoError(err, "listing preserves the host record whose PID is absent locally")
	assert.Equal(t, hostBefore, hostAfter)
}

func TestDaemonAutoStartOverDeadSiblingRecord(t *testing.T) {
	require := require.New(t)
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
	exe, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// Bind only what the helper needs. A recursive bind of / also copies
	// unrelated host mounts, and one that disappears mid-copy fails bwrap.
	args := []string{
		"--unshare-user", "--unshare-pid", "--die-with-parent",
		"--ro-bind", "/usr", "/usr", "--ro-bind-try", "/lib", "/lib", "--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/etc", "/etc", "--ro-bind", filepath.Dir(exe), filepath.Dir(exe),
		"--bind", dataDir, dataDir, "--proc", "/proc", "--",
	}
	daemonPIDNamespaceProbe.Do(func() {
		probe := exec.CommandContext(ctx, bwrap, append(args, exe, "-test.run=^$")...)
		daemonPIDNamespaceProbe.output, daemonPIDNamespaceProbe.err = probe.CombinedOutput()
	})
	if daemonPIDNamespaceProbe.err != nil {
		if os.Getenv("CI") != "" {
			require.NoError(t, daemonPIDNamespaceProbe.err, "user/PID namespaces unavailable in CI:\n%s", daemonPIDNamespaceProbe.output)
		}
		t.Skipf("user/PID namespaces unavailable: %v\n%s", daemonPIDNamespaceProbe.err, daemonPIDNamespaceProbe.output)
	}
	// kit tells namespaces apart by inode number, and the kernel reuses a
	// freed namespace's number. Hold each sandbox's PID namespace open until
	// the test ends so a later sandbox can't inherit it and see this
	// sandbox's records as Local. Bubblewrap reports the sandbox's PID on
	// fd 3, then waits on fd 4 so the namespace is still there to open.
	infoR, infoW, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = infoR.Close() }()
	blockR, blockW, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = blockW.Close() }()
	helperArgs := append([]string{"--info-fd", "3", "--block-fd", "4"}, args...)
	cmd := exec.CommandContext(ctx, bwrap, append(helperArgs, exe, "-test.run=^TestDaemonRuntimePIDNamespaceHelper$", "-test.v")...)
	cmd.Env = append(os.Environ(), "MSGVAULT_HOME="+dataDir, "MSGVAULT_TEST_PIDNS="+mode)
	cmd.ExtraFiles = []*os.File{infoW, blockR}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err = cmd.Start()
	_ = infoW.Close()
	_ = blockR.Close()
	require.NoError(t, err)
	var info struct {
		ChildPID int `json:"child-pid"`
	}
	holdErr := json.NewDecoder(infoR).Decode(&info)
	if holdErr == nil {
		var namespace *os.File
		namespace, holdErr = os.Open(fmt.Sprintf("/proc/%d/ns/pid", info.ChildPID))
		if holdErr == nil {
			t.Cleanup(func() { _ = namespace.Close() })
		}
	}
	_, _ = blockW.Write([]byte{0})
	_ = blockW.Close()
	err = cmd.Wait()
	require.NoError(t, err, "namespace helper (%s):\n%s", mode, output.String())
	require.NoError(t, holdErr, "hold the helper's PID namespace")
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
	if mode != "crash" {
		previous, err := store.Read(path)
		require.NoError(err)
		require.Equal(daemon.ProcessScopeForeign, daemon.RuntimeProcessScope(previous), "exercise Foreign, rather than Unknown or Gone")
		require.True(daemon.ProcessAlive(previous.PID), "foreign PID collides with a live local process")
	}
	if mode == "list" {
		records, err := store.List()
		require.NoError(err)
		for _, rec := range records {
			if rec.PID != os.Getpid() {
				require.Equal(daemon.ProcessScopeForeign, daemon.RuntimeProcessScope(rec))
				require.False(daemon.ProcessAlive(rec.PID), "host producer's PID is absent inside the sandbox")
			}
		}
		records, err = listLiveDaemonRuntimeRecords(dataDir)
		require.NoError(err)
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
		_, launchLock, err := waitForUsableBackgroundRuntimeOrLaunchLock(t.Context(), dataDir, cfg.Server.DaemonAutoRestart, 500*time.Millisecond)
		require.NoError(err)
		require.NotNil(launchLock, "a dead sibling must allow CLI auto-start")
		require.NoError(launchLock.Unlock())
	}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	owner, err := claimServeOwnership(ctx, cfg, "127.0.0.1", 8123, "v-test")
	require.NoError(err, "serve starts over the dead sibling record")
	replacement, err := store.Read(path)
	require.NoError(err)
	assert.Equal(daemon.ProcessScopeLocal, daemon.RuntimeProcessScope(replacement), "serve publishes its own record")
	if mode == "restart" {
		t.Cleanup(func() { require.NoError(owner.Close()) })
	}
}
