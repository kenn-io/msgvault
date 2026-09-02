package peoplesweep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	OpenAIChatProviderVersion       = "openai-chat-completions-json-schema-v1"
	OpenAICompatibleProviderVersion = OpenAIChatProviderVersion
	CodexAppServerProviderVersion   = "codex-app-server-v2"
)

// ErrCodexIsolationUnreleased keeps the app-server provider unavailable until
// its launch containment is implemented and independently release-gated.
var ErrCodexIsolationUnreleased = errors.New("codex app-server isolation is not released")

// DriverRegistry resolves one driver only from the saved protocol. Codex is
// configured at selection time because its executable is operational config,
// not part of the immutable provider profile.
type DriverRegistry struct {
	drivers   map[Protocol]StructuredDriver
	commands  CommandStarter
	isolation CodexIsolationGate
}

func NewDriverRegistry(
	httpClient *http.Client,
	commands CommandStarter,
	isolation CodexIsolationGate,
) (*DriverRegistry, error) {
	return &DriverRegistry{
		drivers: map[Protocol]StructuredDriver{
			ProtocolOpenAIChat:            NewOpenAIChatDriver(httpClient),
			ProtocolOpenAIResponses:       NewOpenAIResponsesDriver(httpClient),
			ProtocolAnthropicMessages:     NewAnthropicMessagesDriver(httpClient),
			ProtocolGoogleGenerateContent: NewGoogleGenerateContentDriver(httpClient),
		},
		commands: commands, isolation: isolation,
	}, nil
}

// capabilityDriver resolves exactly one already-registered HTTP protocol for
// setup negotiation. It deliberately excludes Codex process construction and
// performs no runtime profile, authority, consent, or credential lookup.
func (r *DriverRegistry) capabilityDriver(protocol Protocol) (StructuredDriver, error) {
	if r == nil {
		return nil, errors.New("people inference driver registry is required")
	}
	switch protocol {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses,
		ProtocolAnthropicMessages, ProtocolGoogleGenerateContent:
		driver, ok := r.drivers[protocol]
		if !ok || driver == nil {
			return nil, fmt.Errorf("unsupported people sweep protocol %q", protocol)
		}
		return driver, nil
	default:
		return nil, fmt.Errorf("unsupported people sweep capability protocol %q", protocol)
	}
}

func (r *DriverRegistry) Driver(
	protocol Protocol,
	provider ProviderConfig,
) (StructuredDriver, error) {
	if r == nil {
		return nil, errors.New("people inference driver registry is required")
	}
	validation := Config{
		Enabled: true, Provider: ProviderSelection{Name: "runtime"},
		Providers: map[string]ProviderConfig{"runtime": provider},
	}
	validation.ApplyDefaults()
	if err := validation.Validate(); err != nil {
		return nil, err
	}
	_, canonical, err := validation.ActiveProviderConfig()
	if err != nil {
		return nil, err
	}
	if protocol != canonical.Protocol {
		return nil, errors.New("people inference driver protocol does not match provider configuration")
	}
	if driver, ok := r.drivers[protocol]; ok {
		return driver, nil
	}
	if protocol != ProtocolCodexAppServer {
		return nil, fmt.Errorf("unsupported people sweep protocol %q", protocol)
	}
	if r.commands == nil {
		return nil, errors.New("codex app-server command starter is required")
	}
	if r.isolation == nil {
		return nil, errors.New("codex app-server isolation gate is required")
	}
	attestation, err := r.isolation.Verify(
		context.Background(), canonical.Executable, canonical.ExecutionBoundary,
	)
	if err != nil {
		_ = attestation.Close()
		return nil, fmt.Errorf("verify codex app-server isolation: %w", err)
	}
	if err := attestation.Close(); err != nil {
		return nil, err
	}
	return NewCodexAppServerDriver(canonical, r.commands, r.isolation)
}

// CanonicalCodexProviderVersion derives a safe provider identity from the
// app-server executable attestation without exposing executable paths.
func CanonicalCodexProviderVersion(attestation CodexAttestation) (string, error) {
	digest := strings.ToLower(strings.TrimSpace(attestation.ExecutableSHA256))
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != digest {
		return "", errors.New("codex executable SHA-256 must be 64 lowercase-or-uppercase hexadecimal characters")
	}
	if strings.TrimSpace(attestation.Version) == "" || strings.TrimSpace(attestation.ExecutionBoundary) == "" ||
		strings.TrimSpace(string(attestation.LaunchArtifact)) == "" {
		return "", errors.New("codex attestation version, execution boundary, and launch artifact are required")
	}
	payload, err := json.Marshal(struct {
		Version           string `json:"version"`
		ExecutableSHA256  string `json:"executable_sha256"`
		ExecutionBoundary string `json:"execution_boundary"`
		LaunchArtifact    string `json:"launch_artifact"`
	}{
		Version: attestation.Version, ExecutableSHA256: digest,
		ExecutionBoundary: attestation.ExecutionBoundary,
		LaunchArtifact:    string(attestation.LaunchArtifact),
	})
	if err != nil {
		return "", errors.New("encode codex provider version")
	}
	sum := sha256.Sum256(payload)
	return CodexAppServerProviderVersion + ":" + hex.EncodeToString(sum[:]), nil
}

// DriverVersionMatches reports whether a provider-reported (attested) driver
// identity satisfies a profile's configured driver version. HTTP drivers
// report the configured version verbatim. The Codex app-server driver instead
// reports its attestation-bound identity as "<family>:<attestation-digest>",
// so the configured codex family version is satisfied by that identity only
// when the family prefix matches exactly and the digest suffix is a canonical
// lowercase SHA-256 hexadecimal string. Digest suffixed identities are never
// accepted for any other driver family.
func DriverVersionMatches(configured, attested string) bool {
	if configured == "" || attested == "" {
		return false
	}
	if attested == configured {
		return true
	}
	family, digest, separated := strings.Cut(attested, ":")
	if !separated || family != configured || family != CodexAppServerProviderVersion {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == digest
}
