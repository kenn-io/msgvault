package peoplesweep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var ErrInvalidStructuredOutput = errors.New("inference provider returned invalid structured output")

// SourceDescriptor records the policy-relevant class and calendar date for one
// text source included in a structured request.
type SourceDescriptor struct {
	Class      SourceClass `json:"class"`
	ObservedOn string      `json:"observed_on"`
}

// StructuredRequest is the text-only provider-neutral inference contract.
type StructuredRequest struct {
	ProgramID         string             `json:"program_id"`
	ProgramVersion    string             `json:"program_version"`
	Sources           []SourceDescriptor `json:"sources"`
	ContainsSensitive bool               `json:"contains_sensitive"`
	InputText         string             `json:"input_text"`
	SchemaName        string             `json:"schema_name"`
	JSONSchema        json.RawMessage    `json:"json_schema"`
	MaxOutputTokens   int                `json:"max_output_tokens"`
}

// TokenUsage is provider-reported accounting. It is not trusted as a privacy
// or budget boundary on its own.
type TokenUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// StructuredResponse contains only parsed JSON and safe provider metadata.
type StructuredResponse struct {
	Output            json.RawMessage `json:"output"`
	ProviderRequestID string          `json:"provider_request_id,omitempty"`
	ProviderVersion   string          `json:"provider_version"`
	ModelVersion      string          `json:"model_version"`
	Usage             TokenUsage      `json:"usage"`
}

// StructuredTransport is the network-only adapter boundary. Callers must use
// Runner rather than invoke a transport directly outside this package.
type StructuredTransport interface {
	PrepareJSON(profile ProviderProfile, request StructuredRequest) (PreparedStructuredRequest, error)
	GeneratePreparedJSON(ctx context.Context, profile ProviderProfile, credential string, prepared PreparedStructuredRequest) (StructuredResponse, error)
}

// StructuredRunner is the consent-gated entry point later people-sweep
// programs consume.
type StructuredRunner interface {
	PrepareStructured(ctx context.Context, request StructuredRequest) (PreparedStructuredRequest, error)
	RunPreparedStructured(ctx context.Context, prepared PreparedStructuredRequest) (StructuredResponse, error)
	RunStructured(ctx context.Context, request StructuredRequest) (StructuredResponse, error)
}

// ProviderError exposes only response status and a safe provider request ID.
// Provider response bodies are intentionally discarded.
type ProviderError struct {
	StatusCode int
	RequestID  string
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string {
	if e.RequestID == "" {
		return fmt.Sprintf("inference provider returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("inference provider returned HTTP %d (request_id=%s)",
		e.StatusCode, e.RequestID)
}

// PreparedStructuredRequest owns the exact wire bytes covered by a budget
// reservation. Its fields are private and every accessor returns a copy.
type PreparedStructuredRequest struct {
	request     StructuredRequest
	wireRequest []byte
	wireSHA256  string
	synthetic   bool
	preparedBy  *Runner
}

// NewPreparedStructuredRequest creates an immutable-by-copy request packet.
func NewPreparedStructuredRequest(request StructuredRequest, wireRequest []byte) (PreparedStructuredRequest, error) {
	if _, err := validateStructuredRequest(request, request.ProgramID == "provider-check"); err != nil {
		return PreparedStructuredRequest{}, err
	}
	if len(wireRequest) == 0 {
		return PreparedStructuredRequest{}, errors.New("prepared structured request wire bytes are empty")
	}
	wire := append([]byte(nil), wireRequest...)
	digest := sha256.Sum256(wire)
	return PreparedStructuredRequest{
		request: cloneStructuredRequest(request), wireRequest: wire,
		wireSHA256: hex.EncodeToString(digest[:]),
	}, nil
}

func (p PreparedStructuredRequest) Request() StructuredRequest {
	return cloneStructuredRequest(p.request)
}

func (p PreparedStructuredRequest) WireRequest() []byte { return append([]byte(nil), p.wireRequest...) }

func (p PreparedStructuredRequest) WireSHA256() string { return p.wireSHA256 }

func (p PreparedStructuredRequest) validateWireHash() error {
	if len(p.wireRequest) == 0 || p.wireSHA256 == "" {
		return errors.New("prepared structured request is incomplete")
	}
	digest := sha256.Sum256(p.wireRequest)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), p.wireSHA256) {
		return errors.New("prepared structured request wire hash does not match")
	}
	return nil
}

func cloneStructuredRequest(request StructuredRequest) StructuredRequest {
	request.Sources = append([]SourceDescriptor(nil), request.Sources...)
	request.JSONSchema = append(json.RawMessage(nil), request.JSONSchema...)
	return request
}

// CommandStarter is the future Codex app-server process boundary.
type CommandStarter interface {
	Start(ctx context.Context, executable CodexExecutable, args []string, env []string, dir string) (RPCProcess, error)
}

// CodexExecutable is the launch-only view of an attested executable. Path is
// the canonical source identity; production process creation uses the private
// verified snapshot retained by the attestation.
type CodexExecutable struct {
	sourcePath   string
	verifiedPath string
}

// CodexLaunchArtifact identifies the registry-reviewed way executable bytes
// may be launched without consulting unverified adjacent resources.
type CodexLaunchArtifact string

// CodexLaunchArtifactNativeStandaloneV1 admits only a native executable for
// the running platform with no non-system or executable-relative dependencies.
const CodexLaunchArtifactNativeStandaloneV1 CodexLaunchArtifact = "native-standalone-v1"

// Path returns the canonical source identity without exposing the private
// verified snapshot path.
func (e CodexExecutable) Path() string { return e.sourcePath }

// RPCProcess is the minimal app-server process surface.
type RPCProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait() error
	Kill() error
}

// CodexAttestation binds the executable tested by the isolation gate.
type CodexAttestation struct {
	ExecutablePath     string
	Version            string
	ExecutableSHA256   string
	ExecutionBoundary  string
	LaunchArtifact     CodexLaunchArtifact
	verifiedExecutable *verifiedCodexExecutable
}

// VerifiedExecutable returns the launch-only view owned by this attestation.
func (a CodexAttestation) VerifiedExecutable() CodexExecutable {
	if a.verifiedExecutable == nil {
		return CodexExecutable{sourcePath: a.ExecutablePath}
	}
	return CodexExecutable{
		sourcePath: a.ExecutablePath, verifiedPath: a.verifiedExecutable.path,
	}
}

// Close releases an owned verified executable snapshot. Zero-value and
// synthetic attestations have nothing to release.
func (a CodexAttestation) Close() error {
	if a.verifiedExecutable == nil {
		return nil
	}
	return a.verifiedExecutable.Close()
}

// CodexIsolationGate proves app-server execution remains contained.
type CodexIsolationGate interface {
	Verify(ctx context.Context, executable string, expectedBoundary string) (CodexAttestation, error)
	ReverifyForLaunch(attestation CodexAttestation) error
}
