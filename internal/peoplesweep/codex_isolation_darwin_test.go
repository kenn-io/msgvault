//go:build darwin

package peoplesweep

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const codexSeatbeltDenied = "operation not permitted"

type codexSeatbeltProbe struct {
	t      *testing.T
	stdin  *json.Encoder
	stdout *bufio.Scanner
}

type codexSeatbeltProbeResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Value string `json:"value"`
}

func (p codexSeatbeltProbe) run(operation map[string]string) codexSeatbeltProbeResult {
	p.t.Helper()
	require.NoError(p.t, p.stdin.Encode(operation))
	require.True(p.t, p.stdout.Scan(), "probe exited before answering %v", operation)
	var result codexSeatbeltProbeResult
	require.NoError(p.t, json.Unmarshal(p.stdout.Bytes(), &result))
	return result
}

// denied asserts that the boundary, not a missing target, refused the operation.
func (p codexSeatbeltProbe) denied(operation map[string]string) {
	p.t.Helper()
	result := p.run(operation)
	assert.False(p.t, result.OK, "%v", operation)
	assert.Contains(p.t, result.Error, codexSeatbeltDenied, "%v", operation)
	assert.NotContains(p.t, result.Value, "SYNTHETIC", "%v", operation)
}

func buildCodexSeatbeltProbe(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	// This read makes Go's test cache rerun the test when the child's inputs change.
	_, err := os.ReadDir(filepath.Join("testdata", "codexseatbeltprobe"))
	require.NoError(t, err)
	command := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", path, ".")
	command.Dir = filepath.Join("testdata", "codexseatbeltprobe")
	command.Env = append(os.Environ(), "CGO_ENABLED=1", "GOWORK=off")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	return path
}

func startCodexSeatbeltProbe(t *testing.T, process RPCProcess) codexSeatbeltProbe {
	t.Helper()
	scanner := bufio.NewScanner(process.Stdout())
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return codexSeatbeltProbe{t: t, stdin: json.NewEncoder(process.Stdin()), stdout: scanner}
}

// TestCodexSeatbeltLauncherContainsChild runs a probe in Codex's place
// through the production gate, launcher, proxy and Seatbelt starter. Unlike
// the app server, the probe tries every path itself, so the result shows what
// the profile allows rather than what Codex chooses to do. Each refusal must
// be the sandbox's EPERM, and the same probe outside the sandbox reaches the
// same targets.
func TestCodexSeatbeltLauncherContainsChild(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	probePath := buildCodexSeatbeltProbe(t)
	contents, err := os.ReadFile(probePath)
	requireChecks.NoError(err)
	registry := codexIsolationFixtureRegistry(contents)
	for key, attestation := range registry {
		attestation.Version = "codex-cli 0.0.0-seatbelt-probe"
		registry[key] = attestation
	}

	root := t.TempDir()
	archive := filepath.Join(root, "archive", "msgvault.db")
	requireChecks.NoError(os.MkdirAll(filepath.Dir(archive), 0o700))
	requireChecks.NoError(os.WriteFile(archive, []byte("SYNTHETIC_ARCHIVE_CANARY"), 0o600))
	authHome := filepath.Join(root, "auth")
	requireChecks.NoError(os.Mkdir(authHome, 0o700))
	const syntheticAuth = `{"synthetic_secret":"SYNTHETIC_AUTH_SECRET"}`
	requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), []byte(syntheticAuth), 0o600))
	requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "unrelated.txt"), []byte("SYNTHETIC_UNRELATED_AUTH_FILE"), 0o600))
	ambientHome := filepath.Join(root, "ambient-codex")
	requireChecks.NoError(os.Mkdir(ambientHome, 0o700))
	requireChecks.NoError(os.WriteFile(filepath.Join(ambientHome, "auth.json"), []byte("SYNTHETIC_AMBIENT_AUTH"), 0o600))
	t.Setenv("CODEX_HOME", ambientHome)
	t.Setenv("MSGVAULT_CODEX_SYNTHETIC_ENV_CANARY", "SYNTHETIC_ENV_CANARY")
	// Unix socket paths are limited to 104 bytes, more than TMPDIR leaves.
	socketRoot, err := os.MkdirTemp("/tmp", "mv-codex-") //nolint:usetesting // t.TempDir exceeds the socket path limit
	requireChecks.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	canarySocket := filepath.Join(socketRoot, "canary.sock")
	unixCanary, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", canarySocket)
	requireChecks.NoError(err)
	t.Cleanup(func() { _ = unixCanary.Close() })
	tcpCanary, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	requireChecks.NoError(err)
	t.Cleanup(func() { _ = tcpCanary.Close() })
	var canaryAccepted atomic.Bool
	go func() {
		for {
			conn, err := tcpCanary.Accept()
			if err != nil {
				return
			}
			canaryAccepted.Store(true)
			_ = conn.Close()
		}
	}()
	// A preference domain the child must not read through cfprefsd.
	preferenceDomain := "io.kenn.msgvault.seatbelt-test-" + strings.ToLower(filepath.Base(root))
	requireChecks.NoError(exec.CommandContext(t.Context(), "/usr/bin/defaults", "write", preferenceDomain, "SyntheticCanary", "-string", "SYNTHETIC_PREFERENCE").Run())
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "/usr/bin/defaults", "delete", preferenceDomain).Run()
	})

	// The same probe outside the sandbox reaches every target below.
	unconfined := exec.CommandContext(t.Context(), probePath)
	unconfinedIn, err := unconfined.StdinPipe()
	requireChecks.NoError(err)
	unconfinedOut, err := unconfined.StdoutPipe()
	requireChecks.NoError(err)
	requireChecks.NoError(unconfined.Start())
	baseline := codexSeatbeltProbe{t: t, stdin: json.NewEncoder(unconfinedIn), stdout: bufio.NewScanner(unconfinedOut)}
	for _, operation := range []map[string]string{
		{"op": "read", "path": archive},
		{"op": "read", "path": filepath.Join(authHome, "unrelated.txt")},
		{"op": "dial", "network": "tcp", "address": tcpCanary.Addr().String()},
		{"op": "dial", "network": "unix", "address": canarySocket},
		{"op": "preference", "domain": preferenceDomain, "key": "SyntheticCanary"},
	} {
		assertChecks.True(baseline.run(operation).OK, "unconfined baseline %v", operation)
	}
	requireChecks.NoError(unconfinedIn.Close())
	requireChecks.NoError(unconfined.Wait())
	requireChecks.Eventually(canaryAccepted.Load, 2*time.Second, 10*time.Millisecond)
	canaryAccepted.Store(false)

	upstream, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	requireChecks.NoError(err)
	t.Cleanup(func() { _ = upstream.Close() })
	go func() {
		for {
			conn, err := upstream.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	var dialMu sync.Mutex
	var dials []string
	proxy := newCodexHostServiceProxy(func(ctx context.Context, _, address string) (net.Conn, error) {
		dialMu.Lock()
		dials = append(dials, address)
		dialMu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Addr().String())
	})
	launcher := codexBoundLauncher{
		gate: injectedReleasedCodexGate{registry: registry}, starter: NewCodexCommandStarter(), proxy: proxy,
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	attestation, err := launcher.Verify(ctx, probePath)
	requireChecks.NoError(err)
	defer func() { require.NoError(t, attestation.Close()) }()
	process, err := launcher.Start(ctx, attestation, authHome)
	requireChecks.NoError(err)
	owned, ok := process.(*codexOwnedProcess)
	requireChecks.True(ok)
	workRoot, err := filepath.EvalSymlinks(owned.workRoot)
	requireChecks.NoError(err)
	proxyEndpoint, err := url.Parse(owned.proxy.Endpoint())
	requireChecks.NoError(err)
	probe := startCodexSeatbeltProbe(t, process)

	environment := probe.run(map[string]string{"op": "env"})
	requireChecks.True(environment.OK)
	assertChecks.Equal([]string{
		"ALL_PROXY", "CODEX_EXEC_SERVER_URL", "CODEX_HOME", "HOME", "HTTPS_PROXY", "SSL_CERT_FILE", "TMPDIR",
		"all_proxy", "https_proxy",
	}, strings.Split(environment.Value, "\n"))

	// Allowed: the staged credential, the work root, and the proxy.
	staged := probe.run(map[string]string{"op": "read", "path": filepath.Join(workRoot, ".codex", "auth.json")})
	requireChecks.True(staged.OK, staged.Error)
	assertChecks.JSONEq(syntheticAuth, staged.Value)
	scratch := filepath.Join(workRoot, "tmp", "scratch")
	assertChecks.True(probe.run(map[string]string{"op": "write", "path": scratch}).OK)
	assertChecks.Equal("SYNTHETIC_PROBE_WRITE", probe.run(map[string]string{"op": "read", "path": scratch}).Value)
	tunnel := probe.run(map[string]string{"op": "connect", "authority": "chatgpt.com:443"})
	requireChecks.True(tunnel.OK, tunnel.Error)
	assertChecks.Equal("HTTP/1.1 200 Connection Established", tunnel.Value)
	refused := probe.run(map[string]string{"op": "connect", "authority": "example.com:443"})
	assertChecks.Equal("HTTP/1.1 403 Forbidden", refused.Value)
	dialMu.Lock()
	assertChecks.Equal([]string{"chatgpt.com:443"}, dials)
	dialMu.Unlock()

	// Denied: archive, credentials and other temporary files.
	for _, path := range []string{
		archive, filepath.Join(authHome, "auth.json"), filepath.Join(authHome, "unrelated.txt"),
		filepath.Join(ambientHome, "auth.json"),
	} {
		probe.denied(map[string]string{"op": "read", "path": path})
	}
	probe.denied(map[string]string{"op": "list", "path": filepath.Dir(workRoot)})
	probe.denied(map[string]string{"op": "list", "path": root})
	outside := filepath.Join(root, "written-from-sandbox")
	probe.denied(map[string]string{"op": "write", "path": outside})
	assertChecks.NoFileExists(outside)
	probe.denied(map[string]string{"op": "exec", "path": "/bin/sh"})
	// cfprefsd answers a denied domain as an empty one.
	preference := probe.run(map[string]string{"op": "preference", "domain": preferenceDomain, "key": "SyntheticCanary"})
	assertChecks.False(preference.OK)
	assertChecks.Equal("preference unavailable", preference.Error)

	// Denied: every network path except the proxy port.
	for _, target := range []map[string]string{
		{"op": "dial", "network": "tcp", "address": tcpCanary.Addr().String()},
		{"op": "dial", "network": "tcp", "address": "192.0.2.1:443"},
		{"op": "dial", "network": "udp", "address": "127.0.0.1:53"},
		{"op": "dial", "network": "unix", "address": canarySocket},
		{"op": "dial", "network": "unix", "address": "/private/var/run/mDNSResponder"},
	} {
		probe.denied(target)
	}
	assertChecks.False(canaryAccepted.Load())
	// The profile admits localhost on the proxy's port, so the proxy holds the
	// IPv6 twin of that port: no other process can listen there.
	_, err = (&net.ListenConfig{}).Listen(t.Context(), "tcp6", net.JoinHostPort("::1", proxyEndpoint.Port()))
	requireChecks.ErrorIs(err, syscall.EADDRINUSE)

	requireChecks.NoError(process.Stdin().Close())
	requireChecks.NoError(process.Wait())
	assertChecks.NoDirExists(workRoot)
	contentsAfter, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
	requireChecks.NoError(err)
	assertChecks.JSONEq(syntheticAuth, string(contentsAfter))
}

func TestCodexSeatbeltStarterRejectsUnreviewedLaunch(t *testing.T) {
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	requireChecks.NoError(os.Chmod(workRoot, 0o700))
	publicRoot := t.TempDir()
	requireChecks.NoError(os.Chmod(publicRoot, 0o755))
	executable := CodexExecutable{verifiedPath: "/usr/bin/true"}
	validProxy := "http://msgvault:" + strings.Repeat("a", 64) + "@127.0.0.1:3128"
	starter := seatbeltCodexStarter{}
	for _, tc := range []struct {
		name     string
		args     []string
		env      []string
		dir      string
		endpoint string
		want     string
	}{
		{name: "other command", args: []string{"exec"}, dir: workRoot, want: "reviewed app-server command"},
		{name: "inherited environment", env: []string{"HOME=/"}, dir: workRoot, want: "inherited environment"},
		{name: "relative work root", dir: "work", want: "absolute verified executable"},
		{name: "public work root", dir: publicRoot, want: "private daemon-owned"},
		{name: "remote proxy", dir: workRoot, endpoint: "http://msgvault:" + strings.Repeat("a", 64) + "@192.0.2.1:3128", want: "loopback proxy"},
		{name: "proxy without credential", dir: workRoot, endpoint: "http://127.0.0.1:3128", want: "loopback proxy"},
		{name: "proxy with short credential", dir: workRoot, endpoint: "http://msgvault:short@127.0.0.1:3128", want: "loopback proxy"},
		{name: "https proxy", dir: workRoot, endpoint: strings.Replace(validProxy, "http:", "https:", 1), want: "loopback proxy"},
		{name: "proxy with path", dir: workRoot, endpoint: validProxy + "/tunnel", want: "loopback proxy"},
		{name: "proxy port zero", dir: workRoot, endpoint: strings.Replace(validProxy, ":3128", ":0", 1), want: "loopback proxy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if args == nil {
				args = slices.Clone(codexAppServerArgs)
			}
			var process RPCProcess
			var err error
			if tc.endpoint == "" {
				process, err = starter.Start(t.Context(), executable, args, tc.env, tc.dir)
			} else {
				process, err = starter.StartWithProxy(t.Context(), executable, args, tc.env, tc.dir, tc.endpoint)
			}
			require.ErrorContains(t, err, tc.want)
			assert.Nil(t, process)
		})
	}
}

// TestCodexRegisteredExecutableRejectsAdjacentDynamicDependency catches an
// otherwise-native binary loading unverified code from beside its snapshot,
// including through load commands debug/macho does not decode.
func TestCodexRegisteredExecutableRejectsAdjacentDynamicDependency(t *testing.T) {
	compiler, err := exec.LookPath("cc")
	require.NoError(t, err, "the tagged SQLite build already requires a C compiler")
	for _, tc := range []struct {
		name string
		link []string
	}{
		{name: "strong", link: []string{"-ladjacent"}},
		{name: "weak", link: []string{"-Wl,-weak-ladjacent"}},
		{name: "reexport", link: []string{"-Wl,-reexport-ladjacent"}},
		{name: "loader environment", link: []string{"-Wl,-dyld_env,DYLD_LIBRARY_PATH=@executable_path"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			root := t.TempDir()
			marker := filepath.Join(root, "adjacent-dependency-ran")
			dependencySource := filepath.Join(root, "dependency.c")
			launcherSource := filepath.Join(root, "launcher.c")
			dependency := filepath.Join(root, "libadjacent.dylib")
			executable := filepath.Join(root, "codex-adjacent")
			must.NoError(os.WriteFile(dependencySource, []byte(
				`const char *codex_adjacent(void) { return "adjacent"; }`), 0o600))
			must.NoError(os.WriteFile(launcherSource, []byte(
				"#include <stdio.h>\n"+
					"int main(void) { FILE *f = fopen(\""+marker+"\", \"w\"); "+
					"if (f) { fputs(\"ran\", f); fclose(f); } puts(\"codex-cli 0.149.0\"); return 0; }\n",
			), 0o600))
			compileCodexNativeFixture(t, compiler, "-dynamiclib", dependencySource,
				"-install_name", "@rpath/libadjacent.dylib", "-o", dependency)
			args := append([]string{launcherSource, "-L" + root, "-Wl,-rpath,@loader_path"}, tc.link...)
			compileCodexNativeFixture(t, compiler, append(args, "-o", executable)...)
			contents, err := os.ReadFile(executable)
			must.NoError(err)

			attestation, err := verifyReleasedCodexIsolation(
				t.Context(), executable, CodexExecutionBoundaryV1, codexIsolationFixtureRegistry(contents),
			)
			must.ErrorIs(err, ErrCodexIsolationUnreleased)
			assert.Empty(t, attestation)
			assert.NoFileExists(t, marker)
		})
	}
}

// newCodexPinnedProbeStarter runs the pinned probes through the production
// Seatbelt starter.
func newCodexPinnedProbeStarter(context.Context, *testing.T) CommandStarter {
	return NewCodexCommandStarter()
}

// TestCodexPinnedArtifactDeniesExecutionAndAccessToStagedAuth runs the real
// app server under Seatbelt with a staged synthetic credential, and catches
// its command or filesystem interface answering a client.
func TestCodexPinnedArtifactDeniesExecutionAndAccessToStagedAuth(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if artifact == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	authHome := t.TempDir()
	requireChecks.NoError(os.Chmod(authHome, 0o700))
	const syntheticAuth = `{"synthetic_secret":"SYNTHETIC_AUTH_SECRET"}`
	requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), []byte(syntheticAuth), 0o600))
	launcher := codexBoundLauncher{
		gate: injectedReleasedCodexGate{registry: pinnedCodexProbeRegistry(t, artifact)}, starter: NewCodexCommandStarter(),
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
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
	requireChecks.NoError(client.Notify(ctx, "initialized", nil))
	var models codexModelListResult
	requireChecks.NoError(client.Call(ctx, "model/list", codexModelListRequest().Params, &models))
	requireChecks.NotEmpty(models.Data)
	var thread codexThreadStartResult
	requireChecks.NoError(client.Call(ctx, "thread/start", codexThreadStartRequest(3, ProviderProfile{Model: models.Data[0].ID}).Params, &thread))
	requireChecks.NotEmpty(thread.Thread.ID)
	owned, ok := process.(*codexOwnedProcess)
	requireChecks.True(ok)
	workRoot, err := filepath.EvalSymlinks(owned.workRoot)
	requireChecks.NoError(err)
	assertCodexRejectsExecutionAndFileRequests(ctx, t, client,
		filepath.Join(workRoot, ".codex", "auth.json"), attestation.VerifiedExecutable().verifiedPath, workRoot)
	contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
	requireChecks.NoError(err)
	assertChecks.JSONEq(syntheticAuth, string(contents))
}
