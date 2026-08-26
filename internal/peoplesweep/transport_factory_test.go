package peoplesweep_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

type attestingGate struct {
	calls int
	err   error
}

func (g *attestingGate) Verify(_ context.Context, executable, boundary string) (peoplesweep.CodexAttestation, error) {
	g.calls++
	if g.err != nil {
		return peoplesweep.CodexAttestation{}, g.err
	}
	return peoplesweep.CodexAttestation{
		ExecutablePath:    executable,
		Version:           "1.0.0",
		ExecutableSHA256:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ExecutionBoundary: boundary,
		LaunchArtifact:    peoplesweep.CodexLaunchArtifactNativeStandaloneV1,
	}, nil
}

func (*attestingGate) ReverifyForLaunch(peoplesweep.CodexAttestation) error { return nil }

type noStartCommandStarter struct{}

func (noStartCommandStarter) Start(context.Context, peoplesweep.CodexExecutable, []string, []string, string) (peoplesweep.RPCProcess, error) {
	return nil, errors.New("unexpected process start")
}

func TestStructuredTransportSelectsOpenAICompatible(t *testing.T) {
	transport, err := peoplesweep.NewStructuredTransport(
		validConfig().Provider, http.DefaultClient, nil, nil)
	require.NoError(t, err)
	_, ok := transport.(*peoplesweep.OpenAICompatibleTransport)
	assert.True(t, ok)
}

func TestStructuredTransportSelectsAttestedCodex(t *testing.T) {
	config := validConfig()
	config.Provider = peoplesweep.ProviderConfig{
		Kind: peoplesweep.ProviderCodexAppServer, Model: "gpt-test", ReasoningEffort: "high",
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2025-01-01",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
	}
	config.ApplyDefaults()
	gate := &attestingGate{}

	transport, err := peoplesweep.NewStructuredTransport(config.Provider, http.DefaultClient, noStartCommandStarter{}, gate)
	require.NoError(t, err)
	_, ok := transport.(*peoplesweep.CodexAppServerTransport)
	assert.True(t, ok)
	assert.Equal(t, 1, gate.calls)
}

func TestStructuredTransportCodexFailsClosedWhenAttestationDenied(t *testing.T) {
	config := validConfig()
	config.Provider = peoplesweep.ProviderConfig{
		Kind: peoplesweep.ProviderCodexAppServer, Model: "gpt-test", ReasoningEffort: "high",
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2025-01-01",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
	}
	config.ApplyDefaults()
	gate := &attestingGate{err: peoplesweep.ErrCodexIsolationUnreleased}

	transport, err := peoplesweep.NewStructuredTransport(
		config.Provider, http.DefaultClient, noStartCommandStarter{}, gate,
	)
	require.ErrorIs(t, err, peoplesweep.ErrCodexIsolationUnreleased)
	assert.Nil(t, transport)
	assert.Equal(t, 1, gate.calls)
}

func TestCodexProviderVersionBindsAttestation(t *testing.T) {
	base := peoplesweep.CodexAttestation{
		Version: "1.0.0", ExecutableSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
		LaunchArtifact:    peoplesweep.CodexLaunchArtifactNativeStandaloneV1,
	}
	want, err := peoplesweep.CanonicalCodexProviderVersion(base)
	require.NoError(t, err)
	for _, mutation := range []struct {
		name  string
		apply func(*peoplesweep.CodexAttestation)
	}{
		{"version", func(a *peoplesweep.CodexAttestation) { a.Version = "2.0.0" }},
		{"digest", func(a *peoplesweep.CodexAttestation) {
			a.ExecutableSHA256 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
		}},
		{"boundary", func(a *peoplesweep.CodexAttestation) { a.ExecutionBoundary = "other-boundary" }},
		{"launch artifact", func(a *peoplesweep.CodexAttestation) { a.LaunchArtifact = "other-artifact" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			gotAttestation := base
			mutation.apply(&gotAttestation)
			got, versionErr := peoplesweep.CanonicalCodexProviderVersion(gotAttestation)
			require.NoError(t, versionErr)
			assert.NotEqual(t, want, got)
		})
	}
}
