package peoplesweep_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
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

func TestDriverRegistrySelectsOpenAIChatByProtocol(t *testing.T) {
	config := validConfig()
	registry, err := peoplesweep.NewDriverRegistry(http.DefaultClient, nil, nil)
	require.NoError(t, err)
	driver, err := registry.Driver(peoplesweep.ProtocolOpenAIChat, activeProvider(config))
	require.NoError(t, err)
	_, ok := driver.(*peoplesweep.OpenAIChatDriver)
	assert.True(t, ok)
}

func TestDriverRegistrySelectsOpenAIResponsesByProtocol(t *testing.T) {
	config := configWithProvider(peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIResponses, Endpoint: "https://api.example.test/v1", Model: "gpt-test",
		Auth: peoplesweep.AuthBearer, Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
		OutputMode:       peoplesweep.OutputModeNativeJSONSchema,
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2025-01-01",
	})
	registry, err := peoplesweep.NewDriverRegistry(http.DefaultClient, nil, nil)
	require.NoError(t, err)
	driver, err := registry.Driver(peoplesweep.ProtocolOpenAIResponses, activeProvider(config))
	require.NoError(t, err)
	_, ok := driver.(*peoplesweep.OpenAIResponsesDriver)
	assert.True(t, ok)
}

func TestDriverRegistrySelectsAnthropicMessagesByProtocol(t *testing.T) {
	config := configWithProvider(peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolAnthropicMessages, Endpoint: "https://api.example.test", Model: "claude-test",
		Auth: peoplesweep.AuthXAPIKey, Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
		OutputMode:       peoplesweep.OutputModeNativeJSONSchema,
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2025-01-01",
	})
	registry, err := peoplesweep.NewDriverRegistry(http.DefaultClient, nil, nil)
	require.NoError(t, err)
	driver, err := registry.Driver(peoplesweep.ProtocolAnthropicMessages, activeProvider(config))
	require.NoError(t, err)
	_, ok := driver.(*peoplesweep.AnthropicMessagesDriver)
	assert.True(t, ok)
}

func TestDriverRegistrySelectsGoogleGenerateContentByProtocol(t *testing.T) {
	config := configWithProvider(peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolGoogleGenerateContent, Endpoint: "https://generativelanguage.example.test/v1beta", Model: "gemini-test",
		Auth: peoplesweep.AuthGoogleAPIKey, Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
		OutputMode:       peoplesweep.OutputModeNativeJSONSchema,
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2025-01-01",
	})
	registry, err := peoplesweep.NewDriverRegistry(http.DefaultClient, nil, nil)
	require.NoError(t, err)
	driver, err := registry.Driver(peoplesweep.ProtocolGoogleGenerateContent, activeProvider(config))
	require.NoError(t, err)
	_, ok := driver.(*peoplesweep.GoogleGenerateContentDriver)
	assert.True(t, ok)
}

func TestDriverRegistrySelectsAttestedCodexByProtocol(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	config := validConfig()
	setActiveProvider(&config, peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolCodexAppServer, Model: "gpt-test", ReasoningEffort: "high",
		Auth: peoplesweep.AuthNone, Credential: peoplesweep.CredentialNone,
		OutputMode:       peoplesweep.OutputModeNativeJSONSchema,
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2025-01-01",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
	})
	config.ApplyDefaults()
	gate := &attestingGate{}

	registry, err := peoplesweep.NewDriverRegistry(http.DefaultClient, noStartCommandStarter{}, gate)
	require.NoError(err)
	driver, err := registry.Driver(peoplesweep.ProtocolCodexAppServer, activeProvider(config))
	require.NoError(err)
	_, ok := driver.(*peoplesweep.CodexAppServerDriver)
	assert.True(ok)
	assert.Equal(1, gate.calls)
}

func TestDriverRegistryCodexFailsClosedWhenAttestationDenied(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	config := validConfig()
	setActiveProvider(&config, peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolCodexAppServer, Model: "gpt-test", ReasoningEffort: "high",
		Auth: peoplesweep.AuthNone, Credential: peoplesweep.CredentialNone,
		OutputMode:       peoplesweep.OutputModeNativeJSONSchema,
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2025-01-01",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
	})
	config.ApplyDefaults()
	gate := &attestingGate{err: peoplesweep.ErrCodexIsolationUnreleased}

	registry, err := peoplesweep.NewDriverRegistry(http.DefaultClient, noStartCommandStarter{}, gate)
	require.NoError(err)
	driver, err := registry.Driver(peoplesweep.ProtocolCodexAppServer, activeProvider(config))
	require.ErrorIs(err, peoplesweep.ErrCodexIsolationUnreleased)
	assert.Nil(driver)
	assert.Equal(1, gate.calls)
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

// TestDriverVersionMatchesAcceptsAttestedCodexFamily catches eligibility
// gates comparing a profile's configured driver family against the codex
// driver's attested "<family>:<attestation-digest>" identity with plain
// equality: the codex app-server driver always reports its attestation-bound
// identity, so only the family prefix may be compared while the digest must
// still be validated as a canonical SHA-256 hexadecimal string.
func TestDriverVersionMatchesAcceptsAttestedCodexFamily(t *testing.T) {
	family := peoplesweep.CodexAppServerProviderVersion
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	assert.True(t, peoplesweep.DriverVersionMatches(family, family+":"+digest))
	// HTTP drivers report the configured version verbatim.
	assert.True(t, peoplesweep.DriverVersionMatches(
		peoplesweep.OpenAICompatibleProviderVersion, peoplesweep.OpenAICompatibleProviderVersion))
	// A different but well-formed attestation digest stays eligible: the
	// digest binds the executable attestation, not one fixed build.
	assert.True(t, peoplesweep.DriverVersionMatches(family, family+":"+
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"))
}

func TestDriverVersionMatchesRejectsUnsafeAttestedIdentity(t *testing.T) {
	family := peoplesweep.CodexAppServerProviderVersion
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, test := range []struct{ name, configured, attested string }{
		{"empty configured version", "", family + ":" + digest},
		{"empty attested version", family, ""},
		{"unrelated family", family, "codex-app-server-v1"},
		{"wrong family prefix", family, "codex-app-server-v3:" + digest},
		{"missing digest", family, family + ":"},
		{"short digest", family, family + ":0123456789abcdef"},
		{"long digest", family, family + ":" + digest + digest},
		{"uppercase digest", family, family + ":" + strings.ToUpper(digest)},
		{"non-hex digest", family, family + ":zz23456789abcdef0123456789abcdef0123456789abcdef0123456789ab"},
		{"extra digest segment", family, family + ":" + digest + ":" + digest},
		{
			"non-codex family with digest suffix",
			peoplesweep.OpenAICompatibleProviderVersion,
			peoplesweep.OpenAICompatibleProviderVersion + ":" + digest,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.False(t, peoplesweep.DriverVersionMatches(test.configured, test.attested))
		})
	}
}
