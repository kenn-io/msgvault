package credentialprovider

import (
	"context"
	"encoding/json/v2"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"
	"uuid"
)

// InboundScope names the account msgvault reads and the endpoint that receives
// the credential. SourceType is the msgvault source type, such as "gmail" or
// "msmail". Account is the source identifier, treated as an opaque string.
type InboundScope struct {
	Endpoint   string `json:"endpoint,omitempty"`
	SourceType string `json:"source_type,omitempty"`
	Account    string `json:"account,omitempty"`
}

// InboundRequest asks a helper for a credential msgvault sends to an external
// source endpoint. Renew names the credential it succeeds.
type InboundRequest struct {
	InboundScope

	Version            int       `json:"version"`
	Operation          string    `json:"operation"`
	RequestID          uuid.UUID `json:"request_id"`
	PreviousRequestID  uuid.UUID `json:"previous_request_id,omitzero"`
	CredentialID       uuid.UUID `json:"credential_id,omitzero"`
	ExpectedGeneration int64     `json:"expected_generation,omitzero"`
}

// InboundResponse echoes the exact request scope on ready. A refusal contains
// only the envelope, status and a fixed reason code.
type InboundResponse struct {
	InboundScope

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

var inboundScopeFields = []string{"endpoint", "source_type", "account"}

// DecodeInboundRequest reads one bounded, strict document. Helpers exit 2 on
// invalid input before any remote operation.
func DecodeInboundRequest(reader io.Reader) (InboundRequest, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxDocumentBytes+1))
	if err != nil {
		return InboundRequest{}, ErrInvalidRequest
	}
	var r InboundRequest
	fields, ok := decodeDocument(data, &r)
	if !ok || !validInboundRequest(r) || !fieldsPresent(fields, inboundScopeFields, true) ||
		!fieldsPresent(fields, predecessorFields, r.Operation == "renew") {
		return InboundRequest{}, ErrInvalidRequest
	}
	return r, nil
}

// DecodeInboundResponse validates one complete result against the exact request.
func DecodeInboundResponse(reader io.Reader, request InboundRequest) (InboundResponse, error) {
	if !validInboundRequest(request) {
		return InboundResponse{}, ErrInvalidRequest
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxDocumentBytes+1))
	if err != nil {
		return InboundResponse{}, ErrInvalidResponse
	}
	return decodeInboundResponse(data, request)
}

// WriteInboundResponse checks the complete result before writing. Helpers exit
// nonzero on write failure; clients discard all output from a nonzero exit.
func WriteInboundResponse(writer io.Writer, request InboundRequest, response InboundResponse) error {
	if !validInboundRequest(request) {
		return ErrInvalidRequest
	}
	data, err := json.Marshal(response)
	if err != nil {
		return ErrInvalidResponse
	}
	if _, err := decodeInboundResponse(data, request); err != nil {
		return err
	}
	n, err := writer.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

// ExchangeInbound runs the helper exactly as Exchange does and validates its
// result against request.
func ExchangeInbound(parent context.Context, argv []string, request InboundRequest) (InboundResponse, error) {
	if !validInboundRequest(request) {
		return InboundResponse{}, ErrInvalidRequest
	}
	data, err := json.Marshal(request)
	if err != nil {
		return InboundResponse{}, ErrInvalidRequest
	}
	out, err := exchange(parent, argv, data)
	if err != nil {
		return InboundResponse{}, err
	}
	return decodeInboundResponse(out, request)
}

func validInboundRequest(r InboundRequest) bool {
	if r.Version != 1 || r.RequestID == uuid.Nil() || !validInboundScope(r.InboundScope) {
		return false
	}
	switch r.Operation {
	case "authorize":
		return r.PreviousRequestID == uuid.Nil() && r.CredentialID == uuid.Nil() && r.ExpectedGeneration == 0
	case "renew":
		return r.PreviousRequestID != uuid.Nil() && r.PreviousRequestID != r.RequestID && r.CredentialID != uuid.Nil() && r.ExpectedGeneration > 0
	default:
		return false
	}
}

func validInboundScope(s InboundScope) bool {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(s.Endpoint, "#") {
		return false
	}
	loopback := u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return false
	}
	switch s.SourceType {
	case "gmail", "msmail":
	default:
		return false
	}
	return s.Account != "" && !strings.ContainsFunc(s.Account, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func decodeInboundResponse(data []byte, request InboundRequest) (InboundResponse, error) {
	var r InboundResponse
	fields, ok := decodeDocument(data, &r)
	if !ok || r.Version != request.Version || r.Operation != request.Operation || r.RequestID != request.RequestID {
		return InboundResponse{}, ErrInvalidResponse
	}
	ready := r.Status == StatusReady
	if !fieldsPresent(fields, inboundScopeFields, ready) || !fieldsPresent(fields, resultFields, ready) ||
		!fieldsPresent(fields, []string{"credential"}, ready) || !fieldsPresent(fields, []string{"reason"}, !ready) {
		return InboundResponse{}, ErrInvalidResponse
	}
	if ready {
		if r.InboundScope != request.InboundScope || r.CredentialID == uuid.Nil() || r.CredentialGeneration <= 0 || r.IssuedAt.IsZero() || !r.ExpiresAt.After(r.IssuedAt) {
			return InboundResponse{}, ErrInvalidResponse
		}
		if request.Operation == "renew" && (r.CredentialGeneration <= request.ExpectedGeneration || r.CredentialID == request.CredentialID) {
			return InboundResponse{}, ErrInvalidResponse
		}
		if r.Credential == nil || r.Credential.Type != "bearer" || !bearerToken.MatchString(r.Credential.Token) {
			return InboundResponse{}, ErrInvalidResponse
		}
		return r, nil
	}
	valid := false
	switch r.Status {
	case StatusReady, StatusAlreadyIssued, StatusReplacementRequired, StatusReleased, StatusDenied:
		return InboundResponse{}, ErrInvalidResponse // Ready was handled above; the rest are not inbound results.
	case StatusSignInRequired:
		valid = r.Reason == "no_current_sign_in"
	case StatusApprovalRequired:
		valid = r.Reason == "account_approval_required"
	case StatusConflict:
		valid = r.Reason == "generation_changed"
	case StatusUnavailable:
		valid = r.Reason == "authority_unavailable"
	}
	if !valid {
		return InboundResponse{}, ErrInvalidResponse
	}
	return r, nil
}
