//go:build linux

package peoplesweep

import (
	"context"
	"encoding/json/jsontext"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexPinnedArtifactProxyBridge(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
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
		requireChecks.NoError(err)
		starter = newCodexCommandStarterWithBridge(bridge, bridgeDigest)
	}
	var dials []string
	var dialMu sync.Mutex
	proxy := newCodexHostServiceProxy(func(_ context.Context, _, address string) (net.Conn, error) {
		dialMu.Lock()
		dials = append(dials, address)
		dialMu.Unlock()
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
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
	var initialized map[string]any
	requireChecks.NoError(client.Call(ctx, "initialize", codexInitializeRequest().Params, &initialized))
	requireChecks.NoError(client.Notify(ctx, "initialized", nil))
	loginCtx, cancelLogin := context.WithTimeout(ctx, 10*time.Second)
	defer cancelLogin()
	var login map[string]any
	_ = client.Call(loginCtx, "account/login/start", map[string]string{"type": "chatgptDeviceCode"}, &login)
	dialMu.Lock()
	loginReachedProxy := slices.Contains(dials, "auth.openai.com:443")
	assertChecks.True(loginReachedProxy, "native device login must use the isolated bridge; approved dials: %v", dials)
	dialMu.Unlock()
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
	require.NoError(t, os.Chmod(bridge, 0o700))
	return bridge
}
