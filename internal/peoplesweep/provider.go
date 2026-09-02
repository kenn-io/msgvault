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
	repair            bool
}

// TokenUsage is provider-reported accounting. It is not trusted as a privacy
// or budget boundary on its own.
type TokenUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// DriverResponse contains one candidate JSON value and normalized safe
// metadata. UsageKnown distinguishes an omitted usage object from reported
// zero usage.
type DriverResponse struct {
	CandidateJSON     json.RawMessage
	ProviderRequestID string
	ProviderVersion   string
	ModelVersion      string
	Usage             TokenUsage
	UsageKnown        bool
}

// StructuredResponse contains only locally validated JSON and safe provider metadata.
type StructuredResponse struct {
	Output            json.RawMessage `json:"output"`
	ProviderRequestID string          `json:"provider_request_id,omitempty"`
	ProviderVersion   string          `json:"provider_version"`
	ModelVersion      string          `json:"model_version"`
	Usage             TokenUsage      `json:"usage"`
	UsageKnown        bool            `json:"usage_known"`
	execution         *runnerExecutionSession
}

// ValidationFailure retains only the bounded candidate and bounded local
// diagnostics needed to prepare one repair. Error never exposes those bytes.
type ValidationFailure struct { //nolint:errname // Public error type retained for compatibility with existing consumers.
	Candidate json.RawMessage
	Errors    []string
	repair    bool
	summary   string
	execution *runnerExecutionSession
}

func (f ValidationFailure) Error() string {
	if f.summary == "" {
		return ErrInvalidStructuredOutput.Error() + ": local validation failed"
	}
	return ErrInvalidStructuredOutput.Error() + ": " + f.summary
}

func (f ValidationFailure) Unwrap() error { return ErrInvalidStructuredOutput }

// StructuredDriver is the one-attempt provider boundary. Callers must use
// Runner rather than invoke a driver directly outside this package.
type StructuredDriver interface {
	Prepare(profile ProviderProfile, request StructuredRequest) (PreparedStructuredRequest, error)
	GeneratePrepared(ctx context.Context, profile ProviderProfile, credential Credential, prepared PreparedStructuredRequest) (DriverResponse, error)
}

// PreparedStructuredCall is an opaque, one-shot provider call whose complete
// non-I/O policy and credential checks succeed before its marker callback.
// Once that callback succeeds, Execute crosses directly into provider I/O.
type PreparedStructuredCall interface {
	Execute(ctx context.Context, markStarted func(context.Context) error) (StructuredResponse, error)
}

// StructuredExecutionSession pins one active profile, driver, and resolved
// credential for a primary call and its optional same-profile repair.
type StructuredExecutionSession interface {
	PrimaryCall(prepared PreparedStructuredRequest) (PreparedStructuredCall, error)
	SemanticValidationFailure(response StructuredResponse) (ValidationFailure, error)
	PrepareRepair(failure ValidationFailure) (PreparedStructuredRequest, error)
	RepairCall(prepared PreparedStructuredRequest) (PreparedStructuredCall, error)
}

// StructuredRunner is the consent-gated entry point later people-sweep
// programs consume.
type StructuredRunner interface {
	PrepareStructured(ctx context.Context, request StructuredRequest) (PreparedStructuredRequest, error)
	PrepareRepair(request StructuredRequest, failure ValidationFailure) (PreparedStructuredRequest, error)
	BeginStructuredExecution(ctx context.Context, primary PreparedStructuredRequest) (StructuredExecutionSession, error)
	RunPreparedStructured(ctx context.Context, prepared PreparedStructuredRequest) (StructuredResponse, error)
	RunStructured(ctx context.Context, request StructuredRequest) (StructuredResponse, error)
}

// ProviderCapabilityError is a bounded classification derived only from a
// protocol's structured error envelope. It never contains provider text.
type ProviderCapabilityError string

const ProviderCapabilityUnsupportedRepresentation ProviderCapabilityError = "unsupported_representation"

// ProviderError exposes only response status, safe request metadata, and an
// optional bounded capability classification. Provider response bodies are
// intentionally discarded.
type ProviderError struct {
	StatusCode int
	RequestID  string
	RetryAfter time.Duration
	Capability ProviderCapabilityError
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
	repair      bool
	preparedBy  *Runner
	identity    *preparedRequestIdentity
	execution   *runnerExecutionSession
}

type preparedRequestIdentity struct{}

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
		wireSHA256: hex.EncodeToString(digest[:]), repair: request.repair,
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
