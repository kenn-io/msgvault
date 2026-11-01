// Package credentialprovider exchanges scoped message MCP credentials with a
// trusted local executable. It does not grant access or implement a login flow.
// The caller retains operation identities, keeps secrets in memory, and owns
// retries. See docs/guides/credential-providers.md for the wire contract.
package credentialprovider

import (
	"encoding/json/v2"
	"errors"
	"io"
	"time"
	"uuid"
)

// MaxDocumentBytes bounds one metadata document, including whitespace. It leaves
// room for hundreds of selected source IDs without accepting archive content.
const MaxDocumentBytes = 16 * 1024

// AttemptTimeout bounds one helper process, including its descendants and I/O.
// Callers may impose a shorter context deadline; retries remain their decision.
const AttemptTimeout = 60 * time.Second

// Status describes a service decision, separate from the helper's exit code.
type Status string

// Supported results distinguish first delivery from committed secretless replay.
const (
	StatusReady               Status = "ready"
	StatusAlreadyIssued       Status = "already_issued"
	StatusReplacementRequired Status = "replacement_required"
	StatusReleased            Status = "released"
	StatusSignInRequired      Status = "sign_in_required"
	StatusApprovalRequired    Status = "approval_required"
	StatusDenied              Status = "denied"
	StatusConflict            Status = "conflict"
	StatusUnavailable         Status = "unavailable"
)

var (
	// ErrInvalidRequest reports malformed input without exposing its contents.
	ErrInvalidRequest = errors.New("invalid message credential provider request")
	// ErrInvalidResponse reports a result that cannot authorize this request.
	ErrInvalidResponse = errors.New("invalid message credential provider response")
	// ErrProviderFailed reports a failed process exchange, not a domain denial.
	ErrProviderFailed = errors.New("message credential provider failed")
)

// Scope identifies the exact approved selection. SourceIDs are ascending decimal
// strings so clients cannot round native int64 IDs. ArchiveID is the native opaque
// archive UID, not a newly allocated identity. Tools is an explicit read subset.
type Scope struct {
	Endpoint     string    `json:"endpoint,omitempty"`
	ArchiveID    string    `json:"archive_id,omitempty"`
	ClientID     uuid.UUID `json:"client_id,omitzero"`
	DelegationID uuid.UUID `json:"delegation_id,omitzero"`
	SourceIDs    []string  `json:"source_ids,omitempty"`
	Tools        []string  `json:"tools,omitempty"`
}

// Request is saved before starting its helper. Renew/replace select the exact
// predecessor; release names the original authorization and never carries a token.
type Request struct {
	Scope

	Version                int       `json:"version"`
	Operation              string    `json:"operation"`
	RequestID              uuid.UUID `json:"request_id"`
	PreviousRequestID      uuid.UUID `json:"previous_request_id,omitzero"`
	CredentialID           uuid.UUID `json:"credential_id,omitzero"`
	ExpectedGeneration     int64     `json:"expected_generation,omitzero"`
	AuthorizationRequestID uuid.UUID `json:"authorization_request_id,omitzero"`
}

// Credential is delivered only on first success. Never save it to a request
// journal, expose it to an agent, or print a Response containing it.
type Credential struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

// Response echoes the exact request scope on ready and committed replay. A
// refusal contains only the envelope, status and a fixed reason code.
type Response struct {
	Scope

	Version              int         `json:"version"`
	Operation            string      `json:"operation"`
	RequestID            uuid.UUID   `json:"request_id"`
	Status               Status      `json:"status"`
	Reason               string      `json:"reason,omitempty"`
	CredentialID         uuid.UUID   `json:"credential_id,omitzero"`
	CredentialGeneration int64       `json:"credential_generation,omitzero"`
	IssuedAt             time.Time   `json:"issued_at,omitzero"`
	ExpiresAt            time.Time   `json:"expires_at,omitzero"`
	Credential           *Credential `json:"credential,omitempty"`
}

// DecodeRequest reads one bounded, strict document. Helpers exit 2 on invalid
// input before any remote operation; parser details never enter diagnostics.
func DecodeRequest(reader io.Reader) (Request, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxDocumentBytes+1))
	if err != nil {
		return Request{}, ErrInvalidRequest
	}
	return decodeRequest(data)
}

// DecodeResponse validates one complete result against the exact request.
// Unmarshaling directly into Response does not enforce this contract.
func DecodeResponse(reader io.Reader, request Request) (Response, error) {
	if !validRequest(request) {
		return Response{}, ErrInvalidRequest
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxDocumentBytes+1))
	if err != nil {
		return Response{}, ErrInvalidResponse
	}
	return decodeResponse(data, request)
}

// WriteResponse checks the complete result before writing. Helpers exit nonzero
// on write failure; clients must discard all output from a nonzero exit.
func WriteResponse(writer io.Writer, request Request, response Response) error {
	if !validRequest(request) {
		return ErrInvalidRequest
	}
	data, err := json.Marshal(response)
	if err != nil {
		return ErrInvalidResponse
	}
	if _, err := decodeResponse(data, request); err != nil {
		return err
	}
	n, err := writer.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}
