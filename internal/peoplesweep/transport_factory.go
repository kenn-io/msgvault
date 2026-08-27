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
	OpenAICompatibleProviderVersion = "openai-chat-completions-json-schema-v1"
	CodexAppServerProviderVersion   = "codex-app-server-v2"
)

// ErrCodexIsolationUnreleased keeps the app-server provider unavailable until
// its launch containment is implemented and independently release-gated.
var ErrCodexIsolationUnreleased = errors.New("codex app-server isolation is not released")

// NewStructuredTransport selects the configured provider adapter. Codex
// construction requires an accepted containment attestation; every later
// process launch obtains a fresh attestation and reverifies it immediately.
func NewStructuredTransport(
	cfg ProviderConfig,
	httpClient *http.Client,
	commands CommandStarter,
	isolation CodexIsolationGate,
) (StructuredTransport, error) {
	validation := Config{Enabled: true, Provider: cfg}
	validation.ApplyDefaults()
	if err := validation.Validate(); err != nil {
		return nil, err
	}
	switch validation.Provider.Kind {
	case ProviderOpenAICompatible:
		return NewOpenAICompatibleTransport(httpClient), nil
	case ProviderCodexAppServer:
		if commands == nil {
			return nil, errors.New("codex app-server command starter is required")
		}
		if isolation == nil {
			return nil, errors.New("codex app-server isolation gate is required")
		}
		attestation, err := isolation.Verify(
			context.Background(), validation.Provider.Executable, validation.Provider.ExecutionBoundary,
		)
		if err != nil {
			_ = attestation.Close()
			return nil, fmt.Errorf("verify codex app-server isolation: %w", err)
		}
		if err := attestation.Close(); err != nil {
			return nil, err
		}
		return NewCodexAppServerTransport(validation.Provider, commands, isolation)
	default:
		return nil, fmt.Errorf("unsupported people sweep provider %q", validation.Provider.Kind)
	}
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
