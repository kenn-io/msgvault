package credentialprovider

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"uuid"
)

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var bearerToken = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

var scopeFields = []string{"endpoint", "archive_id", "client_id", "delegation_id", "source_ids", "tools"}
var predecessorFields = []string{"previous_request_id", "credential_id", "expected_generation"}
var resultFields = []string{"credential_id", "credential_generation", "issued_at", "expires_at"}

func decodeDocument(data []byte, destination any) (map[string]jsontext.Value, bool) {
	if len(data) > MaxDocumentBytes || json.Unmarshal(data, destination, json.RejectUnknownMembers(true)) != nil {
		return nil, false
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return nil, false
	}
	for name, value := range fields {
		if bytes.Equal(value, []byte("null")) {
			return nil, false
		}
		switch name {
		case "request_id", "previous_request_id", "authorization_request_id", "credential_id", "client_id", "delegation_id":
			var id string
			if json.Unmarshal(value, &id) != nil || !canonicalUUID.MatchString(id) {
				return nil, false
			}
		}
	}
	return fields, true
}

func fieldsPresent(fields map[string]jsontext.Value, names []string, want bool) bool {
	for _, name := range names {
		_, present := fields[name]
		if present != want {
			return false
		}
	}
	return true
}

func decodeRequest(data []byte) (Request, error) {
	var r Request
	fields, ok := decodeDocument(data, &r)
	if !ok || !validRequest(r) || !fieldsPresent(fields, scopeFields, true) ||
		!fieldsPresent(fields, predecessorFields, r.Operation == "renew" || r.Operation == "replace") ||
		!fieldsPresent(fields, []string{"authorization_request_id"}, r.Operation == "release") {
		return Request{}, ErrInvalidRequest
	}
	return r, nil
}

func validRequest(r Request) bool {
	if r.Version != 1 || r.RequestID == uuid.Nil() || !validScope(r.Scope) {
		return false
	}
	switch r.Operation {
	case "authorize":
		return r.PreviousRequestID == uuid.Nil() && r.CredentialID == uuid.Nil() && r.ExpectedGeneration == 0 && r.AuthorizationRequestID == uuid.Nil()
	case "renew", "replace":
		return r.PreviousRequestID != uuid.Nil() && r.PreviousRequestID != r.RequestID && r.CredentialID != uuid.Nil() && r.ExpectedGeneration > 0 && r.AuthorizationRequestID == uuid.Nil()
	case "release":
		return r.AuthorizationRequestID != uuid.Nil() && r.AuthorizationRequestID != r.RequestID && r.PreviousRequestID == uuid.Nil() && r.CredentialID == uuid.Nil() && r.ExpectedGeneration == 0
	default:
		return false
	}
}

func validScope(s Scope) bool {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(s.Endpoint, "#") {
		return false
	}
	if s.ArchiveID == "" || strings.ContainsFunc(s.ArchiveID, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) || s.ClientID == uuid.Nil() || s.DelegationID == uuid.Nil() || len(s.SourceIDs) == 0 || len(s.Tools) == 0 {
		return false
	}
	var previous int64
	for _, source := range s.SourceIDs {
		id, err := strconv.ParseInt(source, 10, 64)
		if err != nil || id <= previous || strconv.FormatInt(id, 10) != source {
			return false
		}
		previous = id
	}
	for i, tool := range s.Tools {
		switch tool {
		case "get_message", "list_messages", "list_thread", "search_metadata", "search_message_bodies", "get_stats":
		default:
			return false
		}
		if slices.Contains(s.Tools[:i], tool) {
			return false
		}
	}
	return true
}

func decodeResponse(data []byte, request Request) (Response, error) {
	var r Response
	fields, ok := decodeDocument(data, &r)
	if !ok || r.Version != request.Version || r.Operation != request.Operation || r.RequestID != request.RequestID {
		return Response{}, ErrInvalidResponse
	}
	issued := r.Status == StatusReady || r.Status == StatusAlreadyIssued || r.Status == StatusReplacementRequired
	if !fieldsPresent(fields, scopeFields, issued) || !fieldsPresent(fields, resultFields, issued) ||
		!fieldsPresent(fields, []string{"credential"}, r.Status == StatusReady) ||
		!fieldsPresent(fields, []string{"reason"}, !issued && r.Status != StatusReleased) {
		return Response{}, ErrInvalidResponse
	}
	if issued {
		if request.Operation == "release" || r.Endpoint != request.Endpoint || r.ArchiveID != request.ArchiveID || r.ClientID != request.ClientID || r.DelegationID != request.DelegationID || !slices.Equal(r.SourceIDs, request.SourceIDs) || !slices.Equal(r.Tools, request.Tools) || r.CredentialID == uuid.Nil() || r.CredentialGeneration <= 0 || r.IssuedAt.IsZero() || !r.ExpiresAt.After(r.IssuedAt) {
			return Response{}, ErrInvalidResponse
		}
		if (request.Operation == "renew" || request.Operation == "replace") && (r.CredentialGeneration <= request.ExpectedGeneration || r.CredentialID == request.CredentialID) {
			return Response{}, ErrInvalidResponse
		}
		if r.Status == StatusAlreadyIssued && request.Operation != "authorize" || r.Status == StatusReplacementRequired && request.Operation == "authorize" {
			return Response{}, ErrInvalidResponse
		}
		if r.Status == StatusReady && (r.Credential == nil || r.Credential.Type != "bearer" || !bearerToken.MatchString(r.Credential.Token)) {
			return Response{}, ErrInvalidResponse
		}
		return r, nil
	}
	valid := false
	switch r.Status {
	case StatusReady, StatusAlreadyIssued, StatusReplacementRequired:
		return Response{}, ErrInvalidResponse // Issued results were handled above.
	case StatusReleased:
		valid = request.Operation == "release"
	case StatusSignInRequired:
		valid = r.Reason == "no_current_sign_in"
	case StatusApprovalRequired:
		valid = r.Reason == "delegation_approval_required"
	case StatusDenied:
		valid = r.Reason == "delegation_inactive" || r.Reason == "delegation_expired"
	case StatusConflict:
		valid = r.Reason == "request_mismatch" || r.Reason == "generation_changed" || r.Reason == "binding_ended"
	case StatusUnavailable:
		valid = r.Reason == "authority_unavailable"
	}
	if !valid {
		return Response{}, ErrInvalidResponse
	}
	return r, nil
}
