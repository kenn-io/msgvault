//go:build linux || darwin

package peoplesweep

import (
	"context"
	"encoding/json/jsontext"
	"net"
	"os"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The opt-in probes below run the pinned native artifact through the
// platform's production starter: Bubblewrap on Linux, Seatbelt on macOS.
// MSGVAULT_CODEX_PINNED_EXECUTABLE names the artifact under certification.

// TestCodexPinnedArtifactLoginUsesServiceProxy catches device login reaching
// the network other than through the session's policy proxy.
func TestCodexPinnedArtifactLoginUsesServiceProxy(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if artifact == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	starter := newCodexPinnedProbeStarter(ctx, t)
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
	registry := pinnedCodexProbeRegistry(t, artifact)
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
	assertChecks.True(loginReachedProxy, "native device login must use the contained proxy path; approved dials: %v", dials)
	dialMu.Unlock()
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
	starter := newCodexPinnedProbeStarter(ctx, t)
	registry := pinnedCodexProbeRegistry(t, artifact)
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

// assertCodexRejectsExecutionAndFileRequests catches the app server serving
// its command or filesystem interface to a client, which would expose the
// staged credential beside it. Only synthetic secrets are staged.
func assertCodexRejectsExecutionAndFileRequests(
	ctx context.Context, t *testing.T, client *CodexRPCClient, stagedAuth, executable, workRoot string,
) {
	t.Helper()
	for _, request := range []struct {
		method string
		params any
	}{
		{"fs/readFile", map[string]any{"path": stagedAuth}},
		{"command/exec", map[string]any{"command": []string{executable, "--version"}, "cwd": workRoot}},
	} {
		var result map[string]any
		err := client.Call(ctx, request.method, request.params, &result)
		require.ErrorContains(t, err, "failed with code", request.method)
		assert.Empty(t, result, request.method)
		assert.NotContains(t, err.Error(), "SYNTHETIC_AUTH_SECRET")
	}
}

// pinnedCodexProbeRegistry admits the artifact under test at the version it
// reports, after the native standalone contract has admitted its snapshot.
func pinnedCodexProbeRegistry(t *testing.T, artifact string) map[CodexReleaseKey]CodexAttestation {
	t.Helper()
	_, digest, version, err := inspectCodexIsolationCandidate(t.Context(), artifact)
	require.NoError(t, err)
	return map[CodexReleaseKey]CodexAttestation{
		{ExecutableSHA256: digest, ExecutionBoundary: CodexExecutionBoundaryV1}: {
			Version: version, ExecutableSHA256: digest,
			ExecutionBoundary: CodexExecutionBoundaryV1, LaunchArtifact: CodexLaunchArtifactNativeStandaloneV1,
		},
	}
}

func compileCodexNativeFixture(t *testing.T, compiler string, args ...string) {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), compiler, args...).CombinedOutput()
	require.NoError(t, err, string(output))
}
