//go:build linux

package peoplesweep

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCodexRegisteredExecutableRejectsAdjacentDynamicDependency catches an
// otherwise-native binary loading unverified code from beside its snapshot.
func TestCodexRegisteredExecutableRejectsAdjacentDynamicDependency(t *testing.T) {
	must := require.New(t)
	compiler, err := exec.LookPath("cc")
	must.NoError(err, "the tagged SQLite build already requires a C compiler")
	root := t.TempDir()
	marker := filepath.Join(root, "adjacent-dependency-ran")
	dependencySource := filepath.Join(root, "dependency.c")
	launcherSource := filepath.Join(root, "launcher.c")
	dependency := filepath.Join(root, "libadjacent.so")
	executable := filepath.Join(root, "codex-adjacent")
	must.NoError(os.WriteFile(dependencySource, []byte(
		`const char *codex_version(void) { return "codex-cli 0.149.0"; }`), 0o600))
	must.NoError(os.WriteFile(launcherSource, []byte(
		"#include <stdio.h>\n"+
			"const char *codex_version(void);\n"+
			"int main(void) { FILE *f = fopen("+strconv.Quote(marker)+", \"w\"); "+
			"if (f) { fputs(\"ran\", f); fclose(f); } puts(codex_version()); return 0; }\n",
	), 0o600))
	compileCodexNativeFixture(t, compiler, "-shared", "-fPIC", dependencySource, "-o", dependency)
	compileCodexNativeFixture(
		t, compiler, launcherSource, "-L"+root, "-ladjacent", "-Wl,-rpath,$ORIGIN", "-o", executable,
	)
	contents, err := os.ReadFile(executable)
	must.NoError(err)

	attestation, err := verifyReleasedCodexIsolation(
		t.Context(), executable, CodexExecutionBoundaryV1, codexIsolationFixtureRegistry(contents),
	)
	must.ErrorIs(err, ErrCodexIsolationUnreleased)
	assert.Empty(t, attestation)
	assert.NoFileExists(t, marker)
}

func TestCodexPinnedArtifactRunsInIsolatedNetworkNamespace(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	path := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if path == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	process, err := NewCodexCommandStarter().Start(
		ctx, CodexExecutable{verifiedPath: path}, codexAppServerArgs, nil, t.TempDir(),
	)
	requireChecks.NoError(err)
	client := &CodexRPCClient{Process: process}
	defer func() { require.NoError(t, finishCodexProcess(ctx, process, client, true)) }()
	var initialized map[string]any
	initErr := client.Call(ctx, "initialize", codexInitializeRequest().Params, &initialized)
	if initErr != nil {
		stderr, _ := io.ReadAll(io.LimitReader(process.Stderr(), 4096))
		waitErr := process.Wait()
		requireChecks.NoError(initErr, "stderr: %q; wait: %v", stderr, waitErr)
	}
	requireChecks.NoError(client.Notify(ctx, "initialized", nil))
	var models codexModelListResult
	requireChecks.NoError(client.Call(ctx, "model/list", codexModelListRequest(2).Params, &models))
	assertChecks.NotEmpty(models.Data)
	execProcess, ok := process.(*execRPCProcess)
	requireChecks.True(ok)
	actual, err := codexProcessNetworkNamespaces(execProcess.command.Process.Pid)
	requireChecks.NoError(err)
	host, err := os.Readlink("/proc/self/ns/net")
	requireChecks.NoError(err)
	for _, namespace := range actual {
		if namespace != host {
			return
		}
	}
	assertChecks.Fail("Codex remained in the host network namespace")
}

func TestCodexPinnedArtifactDoesNotInheritAmbientAuthHome(t *testing.T) {
	requireChecks := require.New(t)
	path := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if path == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	ordinaryHome := t.TempDir()
	requireChecks.NoError(os.WriteFile(filepath.Join(ordinaryHome, "auth.json"), []byte("{}"), 0o600))
	requireChecks.NoError(os.WriteFile(filepath.Join(ordinaryHome, "sentinel.txt"), []byte("SYNTHETIC_OUTSIDE_PACKET_ROOT"), 0o600))
	t.Setenv("CODEX_HOME", ordinaryHome)
	digest, err := hashCodexExecutable(path)
	requireChecks.NoError(err)
	registry := map[CodexReleaseKey]CodexAttestation{
		{ExecutableSHA256: digest, ExecutionBoundary: CodexExecutionBoundaryV1}: {
			Version: "codex-cli 0.156.0", ExecutableSHA256: digest,
			ExecutionBoundary: CodexExecutionBoundaryV1, LaunchArtifact: CodexLaunchArtifactNativeStandaloneV1,
		},
	}
	launcher := codexBoundLauncher{
		gate: injectedReleasedCodexGate{registry: registry}, starter: NewCodexCommandStarter(),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	attestation, err := launcher.Verify(ctx, path)
	requireChecks.NoError(err)
	defer func() { require.NoError(t, attestation.Close()) }()
	process, err := launcher.Start(ctx, attestation, "")
	requireChecks.NoError(err)
	client := &CodexRPCClient{Process: process}
	defer func() { require.NoError(t, finishCodexProcess(ctx, process, client, true)) }()
	var initialized map[string]any
	requireChecks.NoError(client.Call(ctx, "initialize", codexInitializeRequest().Params, &initialized))
	owned, ok := process.(*codexOwnedProcess)
	requireChecks.True(ok)
	execProcess, ok := owned.RPCProcess.(*execRPCProcess)
	requireChecks.True(ok)
	pids, err := codexProcessTreePIDs(execProcess.command.Process.Pid)
	requireChecks.NoError(err)
	hostNetwork, err := os.Readlink("/proc/self/ns/net")
	requireChecks.NoError(err)
	checked := false
	for _, pid := range pids {
		namespace, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns/net"))
		requireChecks.NoError(err)
		if namespace == hostNetwork {
			continue
		}
		checked = true
		_, err = os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "root/work/.codex/auth.json"))
		requireChecks.ErrorIs(err, os.ErrNotExist)
		_, err = os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "root", strings.TrimPrefix(ordinaryHome, "/"), "sentinel.txt"))
		requireChecks.ErrorIs(err, os.ErrNotExist)
	}
	requireChecks.True(checked, "no isolated Codex child was observed")
}

func TestCodexLauncherStagesOnlyExplicitAuthFile(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if artifact == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	authHome := t.TempDir()
	requireChecks.NoError(os.Chmod(authHome, 0o700))
	requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), []byte("{}"), 0o600))
	requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "unrelated.txt"), []byte("SYNTHETIC_UNRELATED_AUTH_FILE"), 0o600))
	ambientHome := t.TempDir()
	requireChecks.NoError(os.WriteFile(filepath.Join(ambientHome, "auth.json"), []byte("SYNTHETIC_AMBIENT_AUTH"), 0o600))
	t.Setenv("CODEX_HOME", ambientHome)
	digest, err := hashCodexExecutable(artifact)
	requireChecks.NoError(err)
	registry := map[CodexReleaseKey]CodexAttestation{
		{ExecutableSHA256: digest, ExecutionBoundary: CodexExecutionBoundaryV1}: {
			Version: "codex-cli 0.156.0", ExecutableSHA256: digest,
			ExecutionBoundary: CodexExecutionBoundaryV1, LaunchArtifact: CodexLaunchArtifactNativeStandaloneV1,
		},
	}
	launcher := codexBoundLauncher{gate: injectedReleasedCodexGate{registry: registry}, starter: NewCodexCommandStarter()}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	attestation, err := launcher.Verify(ctx, artifact)
	requireChecks.NoError(err)
	defer func() { require.NoError(t, attestation.Close()) }()
	process, err := launcher.Start(ctx, attestation, authHome)
	requireChecks.NoError(err)
	client := &CodexRPCClient{Process: process}
	defer func() { require.NoError(t, finishCodexProcess(ctx, process, client, true)) }()
	var initialized map[string]any
	requireChecks.NoError(client.Call(ctx, "initialize", codexInitializeRequest().Params, &initialized))
	owned, ok := process.(*codexOwnedProcess)
	requireChecks.True(ok)
	execProcess, ok := owned.RPCProcess.(*execRPCProcess)
	requireChecks.True(ok)
	pids, err := codexProcessTreePIDs(execProcess.command.Process.Pid)
	requireChecks.NoError(err)
	hostNetwork, err := os.Readlink("/proc/self/ns/net")
	requireChecks.NoError(err)
	checked := false
	for _, pid := range pids {
		namespace, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns/net"))
		requireChecks.NoError(err)
		if namespace == hostNetwork {
			continue
		}
		checked = true
		root := filepath.Join("/proc", strconv.Itoa(pid), "root/work")
		contents, err := os.ReadFile(filepath.Join(root, ".codex/auth.json"))
		requireChecks.NoError(err)
		assertChecks.Equal("{}", string(contents))
		assertChecks.NoFileExists(filepath.Join(root, "unrelated.txt"))
		assertChecks.NoFileExists(filepath.Join(root, ".codex/unrelated.txt"))
	}
	requireChecks.True(checked, "no isolated Codex child was observed")
	contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
	requireChecks.NoError(err)
	assertChecks.Equal("{}", string(contents))
}

func codexProcessTreePIDs(pid int) ([]int, error) {
	seen := map[int]struct{}{}
	var pids []int
	var visit func(int) error
	visit = func(current int) error {
		if _, ok := seen[current]; ok {
			return nil
		}
		seen[current] = struct{}{}
		pids = append(pids, current)
		root := "/proc/" + strconv.Itoa(current)
		children, err := os.ReadFile(filepath.Join(root, "task", strconv.Itoa(current), "children"))
		if err != nil {
			return err
		}
		for field := range strings.FieldsSeq(string(children)) {
			child, err := strconv.Atoi(field)
			if err != nil {
				return fmt.Errorf("parse Codex child PID: %w", err)
			}
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	err := visit(pid)
	return pids, err
}

func codexProcessNetworkNamespaces(pid int) ([]string, error) {
	seen := map[int]struct{}{}
	var namespaces []string
	var visit func(int) error
	visit = func(current int) error {
		if _, ok := seen[current]; ok {
			return nil
		}
		seen[current] = struct{}{}
		root := "/proc/" + strconv.Itoa(current)
		namespace, err := os.Readlink(filepath.Join(root, "ns/net"))
		if err != nil {
			return err
		}
		namespaces = append(namespaces, namespace)
		children, err := os.ReadFile(filepath.Join(root, "task", strconv.Itoa(current), "children"))
		if err != nil {
			return err
		}
		for field := range strings.FieldsSeq(string(children)) {
			child, err := strconv.Atoi(field)
			if err != nil {
				return fmt.Errorf("parse Codex child PID: %w", err)
			}
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	err := visit(pid)
	return namespaces, err
}

func compileCodexNativeFixture(t *testing.T, compiler string, args ...string) {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), compiler, args...).CombinedOutput()
	require.NoError(t, err, string(output))
}

// TestCodexPinnedStaticPIEArtifact exercises the native release candidate,
// whose ELF type is ET_DYN even though it has no interpreter or libraries.
func TestCodexPinnedStaticPIEArtifact(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	path := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if path == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	digest, err := hashCodexExecutable(path)
	requireChecks.NoError(err)
	assertChecks.Equal("78a11f06e0a2dda42d13fba1d50dc62e8cbdb2d5f69789722f4d4d99b5cdbe30", digest)
	requireChecks.NoError(validateCodexLaunchArtifact(path, CodexLaunchArtifactNativeStandaloneV1))
	version, err := codexExecutableVersion(t.Context(), CodexExecutable{verifiedPath: path})
	requireChecks.NoError(err)
	assertChecks.Equal("codex-cli 0.156.0", version)
}

func TestCodexBridgeFailsClosedWithoutLinkedOrMatchingDigest(t *testing.T) {
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	requireChecks.NoError(os.Chmod(workRoot, 0o700))
	socketPath := filepath.Join(workRoot, codexProxySocketName)
	listener, err := net.Listen("unix", socketPath)
	requireChecks.NoError(err)
	defer func() { require.NoError(t, listener.Close()) }()
	requireChecks.NoError(os.Chmod(socketPath, 0o600))
	bridgePath := filepath.Join(t.TempDir(), "bridge")
	requireChecks.NoError(os.WriteFile(bridgePath, []byte("SYNTHETIC_BAD_BRIDGE"), 0o700))
	for _, tc := range []struct {
		name    string
		starter bubblewrapCodexStarter
		want    string
		wantErr error
	}{
		{name: "unlinked", starter: bubblewrapCodexStarter{bridgePath: bridgePath}, want: "digest is not linked"},
		{name: "mismatch", starter: bubblewrapCodexStarter{bridgePath: bridgePath, bridgeDigest: strings.Repeat("0", 64)}, want: "digest does not match"},
		{name: "missing", starter: bubblewrapCodexStarter{bridgePath: filepath.Join(workRoot, "missing-bridge"), bridgeDigest: strings.Repeat("0", 64)}, wantErr: os.ErrNotExist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			process, err := tc.starter.StartWithProxy(t.Context(), CodexExecutable{verifiedPath: "/bin/true"}, codexAppServerArgs, nil, workRoot, socketPath)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
			assert.Nil(t, process)
		})
	}
}
