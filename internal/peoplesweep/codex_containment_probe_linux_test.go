//go:build linux

package peoplesweep

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type codexContainmentProbeResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// TestCodexPinnedArtifactNegativeContainmentProbes sends command/exec to the
// real pinned app-server through the production launcher. The command executes
// a static syscall probe from disposable /work; no host archive or credentials
// are supplied. The positive controls prove command execution and file I/O
// actually occurred before the negative checks.
func TestCodexPinnedArtifactNegativeContainmentProbes(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if artifact == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	outsideRoot := t.TempDir()
	hostSentinel := filepath.Join(outsideRoot, "sentinel.txt")
	hostWriteTarget := filepath.Join(outsideRoot, "write-target.txt")
	requireChecks.NoError(os.WriteFile(hostSentinel, []byte("SYNTHETIC_OUTSIDE_SENTINEL"), 0o600))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	requireChecks.NoError(err)
	defer func() { require.NoError(t, listener.Close()) }()

	digest, err := hashCodexExecutable(artifact)
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
	attestation, err := launcher.Verify(ctx, artifact)
	requireChecks.NoError(err)
	defer func() { require.NoError(t, attestation.Close()) }()
	process, err := launcher.Start(ctx, attestation, "")
	requireChecks.NoError(err)
	client := &CodexRPCClient{Process: process}
	defer func() { require.NoError(t, finishCodexProcess(ctx, process, client, true)) }()
	owned, ok := process.(*codexOwnedProcess)
	requireChecks.True(ok)
	workRoot := owned.workRoot
	buildCodexContainmentProbe(ctx, t, workRoot)
	allowedInput := filepath.Join(workRoot, "allowed-input.txt")
	requireChecks.NoError(os.WriteFile(allowedInput, []byte("SYNTHETIC_ALLOWED_INPUT"), 0o600))
	var initialized map[string]any
	requireChecks.NoError(client.Call(ctx, "initialize", codexInitializeRequest().Params, &initialized))
	requireChecks.NoError(client.Notify(ctx, "initialized", nil))

	controlRead := codexRunContainmentProbe(ctx, t, client, "read", "/work/allowed-input.txt")
	assertChecks.Equal("ALLOWED SYNTHETIC_ALLOWED_INPUT", strings.TrimSpace(controlRead.Stdout))
	controlWrite := codexRunContainmentProbe(ctx, t, client, "write", "/work/allowed-output.txt")
	assertChecks.Equal("ALLOWED", strings.TrimSpace(controlWrite.Stdout))
	written, err := os.ReadFile(filepath.Join(workRoot, "allowed-output.txt"))
	requireChecks.NoError(err)
	assertChecks.Equal("SYNTHETIC_PROBE_WRITTEN", string(written))

	read := codexRunContainmentProbe(ctx, t, client, "read", hostSentinel)
	assertChecks.Equal("DENIED errno=2", strings.TrimSpace(read.Stdout))
	write := codexRunContainmentProbe(ctx, t, client, "write", hostWriteTarget)
	assertChecks.Equal("DENIED errno=2", strings.TrimSpace(write.Stdout))
	assertChecks.NoFileExists(hostWriteTarget)
	unchanged, err := os.ReadFile(hostSentinel)
	requireChecks.NoError(err)
	assertChecks.Equal("SYNTHETIC_OUTSIDE_SENTINEL", string(unchanged))

	egress := codexRunContainmentProbe(ctx, t, client, "egress", listener.Addr().String())
	assertChecks.Regexp(`^DENIED errno=[0-9]+$`, strings.TrimSpace(egress.Stdout))
	assertChecks.NotEqual("DENIED errno=0", strings.TrimSpace(egress.Stdout))
	tcpListener, ok := listener.(*net.TCPListener)
	requireChecks.True(ok)
	requireChecks.NoError(tcpListener.SetDeadline(time.Now().Add(250 * time.Millisecond)))
	connection, acceptErr := tcpListener.Accept()
	if connection != nil {
		_ = connection.Close()
	}
	var networkError net.Error
	requireChecks.ErrorAs(acceptErr, &networkError, "host loopback listener must receive no connection")
	assertChecks.True(networkError.Timeout())
	t.Logf("read=%q write=%q egress=%q; control-read=%q control-write=%q",
		strings.TrimSpace(read.Stdout), strings.TrimSpace(write.Stdout), strings.TrimSpace(egress.Stdout),
		strings.TrimSpace(controlRead.Stdout), strings.TrimSpace(controlWrite.Stdout))
}

func TestCodexPinnedArtifactProxyBridge(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if artifact == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	outsideRoot := t.TempDir()
	hostSentinel := filepath.Join(outsideRoot, "sentinel.txt")
	hostWriteTarget := filepath.Join(outsideRoot, "write-target.txt")
	requireChecks.NoError(os.WriteFile(hostSentinel, []byte("SYNTHETIC_OUTSIDE_SENTINEL"), 0o600))
	var starter CommandStarter
	if os.Getenv("MSGVAULT_CODEX_TEST_DEFAULT_BRIDGE") == "1" {
		starter = NewCodexCommandStarter()
	} else {
		bridge := buildCodexProxyBridge(ctx, t)
		bridgeDigest, err := hashCodexExecutable(bridge)
		requireChecks.NoError(err)
		starter = newCodexCommandStarterWithBridge(bridge, bridgeDigest)
	}
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	requireChecks.NoError(err)
	defer func() { require.NoError(t, upstream.Close()) }()
	var dials []string
	var dialMu sync.Mutex
	proxy := newCodexHostServiceProxy(func(ctx context.Context, network, address string) (net.Conn, error) {
		dialMu.Lock()
		dials = append(dials, address)
		dialMu.Unlock()
		if address == "auth.openai.com:443" {
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		}
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Addr().String())
	})
	digest, err := hashCodexExecutable(artifact)
	requireChecks.NoError(err)
	registry := map[CodexReleaseKey]CodexAttestation{
		{ExecutableSHA256: digest, ExecutionBoundary: CodexExecutionBoundaryV1}: {
			Version: "codex-cli 0.156.0", ExecutableSHA256: digest,
			ExecutionBoundary: CodexExecutionBoundaryV1, LaunchArtifact: CodexLaunchArtifactNativeStandaloneV1,
		},
	}
	launcher := codexBoundLauncher{
		gate:    injectedReleasedCodexGate{registry: registry},
		starter: starter, proxy: proxy,
	}
	attestation, err := launcher.Verify(ctx, artifact)
	requireChecks.NoError(err)
	defer func() { require.NoError(t, attestation.Close()) }()
	process, err := launcher.Start(ctx, attestation, "")
	requireChecks.NoError(err)
	client := &CodexRPCClient{Process: process}
	defer func() { require.NoError(t, finishCodexProcess(ctx, process, client, true)) }()
	owned, ok := process.(*codexOwnedProcess)
	requireChecks.True(ok)
	buildCodexContainmentProbe(ctx, t, owned.workRoot)
	var initialized map[string]any
	requireChecks.NoError(client.Call(ctx, "initialize", codexInitializeRequest().Params, &initialized))
	requireChecks.NoError(client.Notify(ctx, "initialized", nil))
	denied := codexRunContainmentProbe(ctx, t, client, "proxy", "example.com:443")
	assertChecks.Equal("DENIED HTTP/1.1 403 Forbidden", strings.TrimSpace(denied.Stdout))
	allowed := codexRunContainmentProbe(ctx, t, client, "proxy", "chatgpt.com:443")
	assertChecks.Equal("ALLOWED HTTP/1.1 200 Connection Established", strings.TrimSpace(allowed.Stdout))
	dialMu.Lock()
	assertChecks.Equal([]string{"chatgpt.com:443"}, dials)
	dialMu.Unlock()
	direct := codexRunContainmentProbe(ctx, t, client, "egress", upstream.Addr().String())
	assertChecks.Regexp(`^DENIED errno=[0-9]+$`, strings.TrimSpace(direct.Stdout))
	read := codexRunContainmentProbe(ctx, t, client, "read", hostSentinel)
	assertChecks.Equal("DENIED errno=2", strings.TrimSpace(read.Stdout))
	write := codexRunContainmentProbe(ctx, t, client, "write", hostWriteTarget)
	assertChecks.Equal("DENIED errno=2", strings.TrimSpace(write.Stdout))
	assertChecks.NoFileExists(hostWriteTarget)
	loginCtx, cancelLogin := context.WithTimeout(ctx, 10*time.Second)
	defer cancelLogin()
	var login map[string]any
	_ = client.Call(loginCtx, "account/login/start", map[string]string{"type": "chatgptDeviceCode"}, &login)
	dialMu.Lock()
	loginReachedProxy := slices.Contains(dials, "auth.openai.com:443")
	assertChecks.True(loginReachedProxy, "native device login must use the isolated bridge; approved dials: %v", dials)
	dialMu.Unlock()
	t.Logf("proxy denied=%q allowed=%q direct-egress=%q read=%q write=%q; native login reached approved proxy=%t",
		strings.TrimSpace(denied.Stdout), strings.TrimSpace(allowed.Stdout), strings.TrimSpace(direct.Stdout),
		strings.TrimSpace(read.Stdout), strings.TrimSpace(write.Stdout), loginReachedProxy)
}

// TestCodexPinnedArtifactAuthTLSRoots performs only a TLS handshake to the
// approved auth authority through the production launcher. It sends no HTTP
// request and cannot start a device-code ceremony.
func TestCodexPinnedArtifactAuthTLSRoots(t *testing.T) {
	requireChecks := require.New(t)
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if artifact == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	digest, err := hashCodexExecutable(artifact)
	requireChecks.NoError(err)
	registry := map[CodexReleaseKey]CodexAttestation{
		{ExecutableSHA256: digest, ExecutionBoundary: CodexExecutionBoundaryV1}: {
			Version: "codex-cli 0.156.0", ExecutableSHA256: digest,
			ExecutionBoundary: CodexExecutionBoundaryV1, LaunchArtifact: CodexLaunchArtifactNativeStandaloneV1,
		},
	}
	launcher := codexBoundLauncher{
		gate: injectedReleasedCodexGate{registry: registry}, starter: NewCodexCommandStarter(),
		proxy: defaultCodexServiceProxy(),
	}
	attestation, err := launcher.Verify(ctx, artifact)
	requireChecks.NoError(err)
	defer func() { require.NoError(t, attestation.Close()) }()
	process, err := launcher.Start(ctx, attestation, "")
	requireChecks.NoError(err)
	client := &CodexRPCClient{Process: process}
	defer func() { require.NoError(t, finishCodexProcess(ctx, process, client, true)) }()
	owned, ok := process.(*codexOwnedProcess)
	requireChecks.True(ok)
	buildCodexContainmentProbe(ctx, t, owned.workRoot)
	var initialized map[string]any
	requireChecks.NoError(client.Call(ctx, "initialize", codexInitializeRequest().Params, &initialized))
	requireChecks.NoError(client.Notify(ctx, "initialized", nil))
	result := codexRunContainmentProbe(ctx, t, client, "tls", "auth.openai.com:443")
	assert.Equal(t, "TLS_VERIFIED", strings.TrimSpace(result.Stdout))
}

func syntheticCodexProbeAuth(user, workspace, refresh string) []byte {
	claims := fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_user_id":%q,"chatgpt_account_id":%q}}`, user, workspace)
	idToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
	return []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"id_token":%q,"access_token":"synthetic-access","refresh_token":%q,"account_id":%q}}`, idToken, refresh, workspace))
}

// This diagnostic uses synthetic auth and command/exec to exercise the real
// production launcher, staged credential, and post-exit copy-back. It is not
// an authenticated structured inference or a release-gate substitute.
func TestCodexPinnedArtifactRefreshCopyBack(t *testing.T) {
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if artifact == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var starter CommandStarter
	if os.Getenv("MSGVAULT_CODEX_TEST_DEFAULT_BRIDGE") == "1" {
		starter = NewCodexCommandStarter()
	} else {
		bridge := buildCodexProxyBridge(ctx, t)
		bridgeDigest, err := hashCodexExecutable(bridge)
		require.NoError(t, err)
		starter = newCodexCommandStarterWithBridge(bridge, bridgeDigest)
	}
	digest, err := hashCodexExecutable(artifact)
	require.NoError(t, err)
	registry := map[CodexReleaseKey]CodexAttestation{
		{ExecutableSHA256: digest, ExecutionBoundary: CodexExecutionBoundaryV1}: {
			Version: "codex-cli 0.156.0", ExecutableSHA256: digest,
			ExecutionBoundary: CodexExecutionBoundaryV1, LaunchArtifact: CodexLaunchArtifactNativeStandaloneV1,
		},
	}
	launcher := codexBoundLauncher{gate: injectedReleasedCodexGate{registry: registry}, starter: starter, proxy: defaultCodexServiceProxy()}
	for _, tc := range []struct {
		name        string
		user        string
		wantChanged bool
	}{
		{name: "same account", user: "user-one"},
		{name: "changed account", user: "user-two", wantChanged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertChecks := assert.New(t)
			requireChecks := require.New(t)
			authHome := t.TempDir()
			requireChecks.NoError(os.Chmod(authHome, 0o700))
			initial := syntheticCodexProbeAuth("user-one", "workspace-one", "old-refresh")
			candidate := syntheticCodexProbeAuth(tc.user, "workspace-one", "new-refresh")
			requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), initial, 0o600))
			attestation, err := launcher.Verify(ctx, artifact)
			requireChecks.NoError(err)
			defer func() { require.NoError(t, attestation.Close()) }()
			process, err := launcher.Start(ctx, attestation, authHome)
			requireChecks.NoError(err)
			owned, ok := process.(*codexOwnedProcess)
			requireChecks.True(ok)
			client := &CodexRPCClient{Process: process}
			buildCodexContainmentProbe(ctx, t, owned.workRoot)
			requireChecks.NoError(os.WriteFile(filepath.Join(owned.workRoot, "refresh.json"), candidate, 0o600))
			var initialized map[string]any
			requireChecks.NoError(client.Call(ctx, "initialize", codexInitializeRequest().Params, &initialized))
			requireChecks.NoError(client.Notify(ctx, "initialized", nil))
			result := codexRunContainmentProbe(ctx, t, client, "copy", "/work/refresh.json", "/work/.codex/auth.json")
			assertChecks.Equal("ALLOWED", strings.TrimSpace(result.Stdout))
			requireChecks.NoError(owned.allowRefreshCommit())
			err = finishCodexProcess(ctx, process, client, false)
			if tc.wantChanged {
				requireChecks.ErrorIs(err, ErrCodexAuthAccountChanged)
			} else {
				requireChecks.NoError(err)
			}
			contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
			requireChecks.NoError(err)
			want := candidate
			if tc.wantChanged {
				want = initial
			}
			assertChecks.Equal(sha256.Sum256(want), sha256.Sum256(contents))
			info, err := os.Lstat(filepath.Join(authHome, "auth.json"))
			requireChecks.NoError(err)
			assertChecks.Equal(os.FileMode(0o600), info.Mode().Perm())
			assertChecks.NoDirExists(owned.workRoot)
		})
	}
}

// This opt-in ceremony uses the production model-less enrollment constructor.
// The short-lived URL and code are shown only in the operator's test output.
func TestCodexPinnedArtifactDeviceEnrollment(t *testing.T) {
	requireChecks := require.New(t)
	if os.Getenv("MSGVAULT_CODEX_ENROLL_NOW") != "1" {
		t.Skip("device enrollment requires an explicit operator start")
	}
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	authHome := os.Getenv("MSGVAULT_CODEX_AUTH_HOME")
	requireChecks.NotEmpty(artifact)
	requireChecks.NotEmpty(authHome)
	client, err := NewCodexEnrollmentClient(artifact, authHome, 10*time.Minute)
	requireChecks.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	err = client.StartDeviceLogin(ctx, func(login DeviceLogin) error {
		t.Logf("Official verification URL: %s", login.VerificationURL)
		t.Logf("One-time user code: %s", login.UserCode)
		t.Logf("Local login deadline: %s", login.ExpiresAt.UTC().Format(time.RFC3339))
		return nil
	})
	requireChecks.NoError(err)
	contents, _, err := readPrivateCodexAuth(authHome)
	requireChecks.NoError(err)
	_, err = codexAccountIdentityFromAuth(contents)
	requireChecks.NoError(err)
}

// This opt-in release probe requires a daemon-selected dedicated auth home.
// It sends only a synthetic packet through the production launcher and the
// real allowlisted upstream, then validates the structured response.
func TestCodexPinnedArtifactPacketOnlyStructuredInference(t *testing.T) {
	requireChecks := require.New(t)
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if artifact == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	authHome := os.Getenv("MSGVAULT_CODEX_AUTH_HOME")
	if authHome == "" {
		t.Skip("dedicated daemon-owned Codex auth home is unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var starter CommandStarter
	if os.Getenv("MSGVAULT_CODEX_TEST_DEFAULT_BRIDGE") == "1" {
		starter = NewCodexCommandStarter()
	} else {
		bridge := buildCodexProxyBridge(ctx, t)
		bridgeDigest, err := hashCodexExecutable(bridge)
		requireChecks.NoError(err)
		starter = newCodexCommandStarterWithBridge(bridge, bridgeDigest)
	}
	digest, err := hashCodexExecutable(artifact)
	requireChecks.NoError(err)
	registry := map[CodexReleaseKey]CodexAttestation{
		{ExecutableSHA256: digest, ExecutionBoundary: CodexExecutionBoundaryV1}: {
			Version: "codex-cli 0.156.0", ExecutableSHA256: digest,
			ExecutionBoundary: CodexExecutionBoundaryV1, LaunchArtifact: CodexLaunchArtifactNativeStandaloneV1,
		},
	}
	gate := injectedReleasedCodexGate{registry: registry}
	provider := ProviderConfig{
		Protocol: ProtocolCodexAppServer, Model: "codex-model-pending-discovery", ReasoningEffort: "medium",
		Auth: AuthNone, Credential: CredentialNone, OutputMode: OutputModeNativeJSONSchema,
		RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
		AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "2026-01-01",
		Executable: artifact, ExecutionBoundary: CodexExecutionBoundaryV1, RequestTimeout: 90 * time.Second,
	}
	driver, err := NewCodexAppServerDriverWithAuthHome(provider, starter, gate, authHome)
	requireChecks.NoError(err)
	models, err := driver.ListModels(ctx)
	requireChecks.NoError(err)
	requireChecks.NotEmpty(models)
	provider.Model = models[0].ID
	provider.ReasoningEffort = models[0].DefaultReasoningEffort
	config := Config{Enabled: true, Provider: ProviderSelection{Name: "codex"}, Providers: map[string]ProviderConfig{"codex": provider}}
	config.ApplyDefaults()
	profile, err := config.Profile()
	requireChecks.NoError(err)
	driver, err = NewCodexAppServerDriverWithAuthHome(provider, starter, gate, authHome)
	requireChecks.NoError(err)
	request := StructuredRequest{
		ProgramID: "synthetic", ProgramVersion: "1",
		Sources:   []SourceDescriptor{{Class: SourceConversationText, ObservedOn: "2026-09-23"}},
		InputText: `{"packet":"synthetic-only","instruction":"return ok"}`, SchemaName: "result",
		JSONSchema:      jsontext.Value(`{"type":"object","properties":{"result":{"type":"string","enum":["ok"]}},"required":["result"],"additionalProperties":false}`),
		MaxOutputTokens: 64,
	}
	prepared, err := driver.Prepare(profile, request)
	requireChecks.NoError(err)
	response, err := driver.GeneratePrepared(ctx, profile, Credential{}, prepared)
	requireChecks.NoError(err)
	assert.JSONEq(t, `{"result":"ok"}`, string(response.CandidateJSON))
	t.Log("packet-only structured inference completed through the pinned launcher")
}

func buildCodexProxyBridge(ctx context.Context, t *testing.T) string {
	t.Helper()
	bridge := filepath.Join(t.TempDir(), "msgvault-codex-bridge")
	command := exec.CommandContext(ctx, "go", "build", "-o", bridge, "go.kenn.io/msgvault/cmd/msgvault-codex-bridge")
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	return bridge
}

func buildCodexContainmentProbe(ctx context.Context, t *testing.T, workRoot string) {
	t.Helper()
	probe := filepath.Join(workRoot, "containment-probe")
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source := filepath.Join(filepath.Dir(sourceFile), "testdata", "codex_containment_probe", "main.go")
	command := exec.CommandContext(ctx, "go", "build", "-o", probe, source) //nolint:gosec // Fixed test source and owner-only output.
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func codexRunContainmentProbe(
	ctx context.Context, t *testing.T, client *CodexRPCClient, operation string, targets ...string,
) codexContainmentProbeResult {
	t.Helper()
	var result codexContainmentProbeResult
	argv := append([]string{"/work/containment-probe", operation}, targets...)
	timeoutMs := 2000
	if operation == "tls" {
		timeoutMs = 10000
	}
	err := client.Call(ctx, "command/exec", map[string]any{
		"command":        argv,
		"cwd":            "/work",
		"sandboxPolicy":  map[string]any{"type": "dangerFullAccess"},
		"timeoutMs":      timeoutMs,
		"outputBytesCap": 4096,
	}, &result)
	require.NoError(t, err)
	require.Equal(t, 0, result.ExitCode, "stderr: %q", result.Stderr)
	assert.Empty(t, result.Stderr)
	t.Logf("command/exec argv=%q cwd=/work sandboxPolicy=dangerFullAccess timeoutMs=%d -> exitCode=%d stdout=%q stderr=%q",
		argv, timeoutMs, result.ExitCode, strings.TrimSpace(result.Stdout), strings.TrimSpace(result.Stderr))
	return result
}
