package credentialprovider_test

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/credentialprovider"
)

const requestJSON = `{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111","endpoint":"https://archive.example.org/messages/mcp","archive_id":"archive-a","client_id":"22222222-2222-4222-8222-222222222222","delegation_id":"33333333-3333-4333-8333-333333333333","source_ids":["17","9007199254740993"],"tools":["get_message","get_stats"]}`

const readyJSON = `{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111","status":"ready","endpoint":"https://archive.example.org/messages/mcp","archive_id":"archive-a","client_id":"22222222-2222-4222-8222-222222222222","delegation_id":"33333333-3333-4333-8333-333333333333","source_ids":["17","9007199254740993"],"tools":["get_message","get_stats"],"credential_id":"44444444-4444-4444-8444-444444444444","credential_generation":1,"issued_at":"2026-10-24T15:00:00Z","expires_at":"2026-10-24T15:05:00Z","credential":{"type":"bearer","token":"synthetic-token"}}`

func TestRequestScopeAndOperations(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	r, err := credentialprovider.DecodeRequest(strings.NewReader(requestJSON))
	requirements.NoError(err)
	assertions.Equal([]string{"17", "9007199254740993"}, r.SourceIDs)
	for name, input := range map[string]string{
		"all sources":               strings.Replace(requestJSON, `["17","9007199254740993"]`, `[]`, 1),
		"duplicate sources":         strings.Replace(requestJSON, `"9007199254740993"`, `"17"`, 1),
		"noncanonical source":       strings.Replace(requestJSON, `"17"`, `"017"`, 1),
		"descending sources":        strings.Replace(requestJSON, `["17","9007199254740993"]`, `["18","17"]`, 1),
		"overflow source":           strings.Replace(requestJSON, `9007199254740993`, `9223372036854775808`, 1),
		"unadmitted tool":           strings.Replace(requestJSON, `get_message`, `get_attachment`, 1),
		"duplicate tool":            strings.Replace(requestJSON, `get_stats`, `get_message`, 1),
		"http":                      strings.Replace(requestJSON, `https:`, `http:`, 1),
		"userinfo":                  strings.Replace(requestJSON, `archive.example.org`, `secret@archive.example.org`, 1),
		"query":                     strings.Replace(requestJSON, `/messages/mcp`, `/messages/mcp?token=secret`, 1),
		"duplicate field":           strings.Replace(requestJSON, `"version":1`, `"version":1,"version":1`, 1),
		"unknown field":             strings.Replace(requestJSON, `"version":1`, `"version":1,"candidate_token":"secret"`, 1),
		"null field":                strings.Replace(requestJSON, `"version":1`, `"version":1,"credential_id":null`, 1),
		"trailing document":         requestJSON + `{}`,
		"oversize":                  requestJSON + strings.Repeat(" ", credentialprovider.MaxDocumentBytes),
		"renew without predecessor": strings.Replace(requestJSON, `authorize`, `renew`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			_, err := credentialprovider.DecodeRequest(strings.NewReader(input))
			assertions.ErrorIs(err, credentialprovider.ErrInvalidRequest)
		})
	}
	for _, operation := range []string{"renew", "replace"} {
		r.Operation = operation
		r.PreviousRequestID = uuid.MustParse("55555555-5555-4555-8555-555555555555")
		r.CredentialID = uuid.MustParse("44444444-4444-4444-8444-444444444444")
		r.ExpectedGeneration = 1
		data, err := json.Marshal(r)
		requirements.NoError(err)
		_, err = credentialprovider.DecodeRequest(bytes.NewReader(data))
		requirements.NoError(err)
	}
}

func TestResponseMustMatchRequest(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	r, err := credentialprovider.DecodeRequest(strings.NewReader(requestJSON))
	requirements.NoError(err)
	response, err := credentialprovider.DecodeResponse(strings.NewReader(readyJSON), r)
	requirements.NoError(err)
	assertions.Equal("synthetic-token", response.Credential.Token)
	assertions.Equal(5*time.Minute, response.ExpiresAt.Sub(response.IssuedAt))
	for name, input := range map[string]string{
		"another endpoint":     strings.Replace(readyJSON, `/messages/mcp`, `/mcp`, 1),
		"another archive":      strings.Replace(readyJSON, `archive-a`, `archive-b`, 1),
		"another installation": strings.Replace(readyJSON, `22222222-2222`, `22222223-2222`, 1),
		"another approval":     strings.Replace(readyJSON, `33333333-3333`, `33333334-3333`, 1),
		"another operation":    strings.Replace(readyJSON, `authorize`, `renew`, 1),
		"another request":      strings.Replace(readyJSON, `11111111-1111`, `11111112-1111`, 1),
		"extra source":         strings.Replace(readyJSON, `"17",`, `"16","17",`, 1),
		"different tool":       strings.Replace(readyJSON, `get_stats`, `list_messages`, 1),
		"empty token":          strings.Replace(readyJSON, `synthetic-token`, ``, 1),
		"header injection":     strings.Replace(readyJSON, `synthetic-token`, `synthetic\r\nHeader:value`, 1),
		"expired interval":     strings.Replace(readyJSON, `15:05:00`, `14:59:59`, 1),
		"denial with secret":   `{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111","status":"denied","reason":"delegation_expired","credential":{"type":"bearer","token":"synthetic-token"}}`,
		"replay with secret":   strings.Replace(readyJSON, `"ready"`, `"already_issued"`, 1),
		"renew replay for authorize": strings.NewReplacer(
			`"ready"`, `"replacement_required"`,
			`,"credential":{"type":"bearer","token":"synthetic-token"}`, "",
		).Replace(readyJSON),
	} {
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			_, err := credentialprovider.DecodeResponse(strings.NewReader(input), r)
			requirements.ErrorIs(err, credentialprovider.ErrInvalidResponse)
			assertions.NotContains(err.Error(), "synthetic-token")
		})
	}
}

func TestReplayAndRefusalNeverReturnSecrets(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	r, err := credentialprovider.DecodeRequest(strings.NewReader(requestJSON))
	requirements.NoError(err)
	response, err := credentialprovider.DecodeResponse(strings.NewReader(readyJSON), r)
	requirements.NoError(err)
	response.Status = credentialprovider.StatusAlreadyIssued
	response.Credential = nil
	var out bytes.Buffer
	requirements.NoError(credentialprovider.WriteResponse(&out, r, response))
	assertions.NotContains(out.String(), "synthetic-token")
	assertions.NotContains(out.String(), `"credential":`)
	replayed, err := credentialprovider.DecodeResponse(&out, r)
	requirements.NoError(err)
	assertions.Nil(replayed.Credential)
	assertions.Equal(response.CredentialID, replayed.CredentialID)
	for _, refusal := range []string{
		`"status":"sign_in_required","reason":"no_current_sign_in"`,
		`"status":"approval_required","reason":"delegation_approval_required"`,
		`"status":"denied","reason":"delegation_expired"`,
		`"status":"conflict","reason":"generation_changed"`,
		`"status":"conflict","reason":"binding_ended"`,
		`"status":"unavailable","reason":"authority_unavailable"`,
	} {
		response, err := credentialprovider.DecodeResponse(strings.NewReader(`{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111",`+refusal+`}`), r)
		requirements.NoError(err)
		assertions.Nil(response.Credential)
	}
}

func TestRenewalAndReleaseContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	r, err := credentialprovider.DecodeRequest(strings.NewReader(requestJSON))
	requirements.NoError(err)
	ready, err := credentialprovider.DecodeResponse(strings.NewReader(readyJSON), r)
	requirements.NoError(err)
	authorizeID := r.RequestID
	r.Operation = "renew"
	r.RequestID = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	r.PreviousRequestID = authorizeID
	r.CredentialID = ready.CredentialID
	r.ExpectedGeneration = 1
	ready.Operation, ready.RequestID = r.Operation, r.RequestID
	var out bytes.Buffer
	// Keeping either the old identity or generation cannot represent a successor.
	ready.CredentialGeneration = 2
	requirements.ErrorIs(credentialprovider.WriteResponse(&out, r, ready), credentialprovider.ErrInvalidResponse)
	assertions.Empty(out.Bytes())
	ready.CredentialID = uuid.MustParse("66666666-6666-4666-8666-666666666666")
	ready.CredentialGeneration = 1
	requirements.ErrorIs(credentialprovider.WriteResponse(&out, r, ready), credentialprovider.ErrInvalidResponse)
	ready.CredentialGeneration = 2
	requirements.NoError(credentialprovider.WriteResponse(&out, r, ready))
	out.Reset()
	ready.Status, ready.Credential = credentialprovider.StatusReplacementRequired, nil
	requirements.NoError(credentialprovider.WriteResponse(&out, r, ready))
	assertions.NotContains(out.String(), "synthetic-token")
	r.Operation = "release"
	r.PreviousRequestID, r.CredentialID, r.ExpectedGeneration = uuid.Nil(), uuid.Nil(), 0
	r.AuthorizationRequestID = authorizeID
	data, err := json.Marshal(r)
	requirements.NoError(err)
	_, err = credentialprovider.DecodeRequest(bytes.NewReader(data))
	requirements.NoError(err)
	out.Reset()
	requirements.NoError(credentialprovider.WriteResponse(&out, r, credentialprovider.Response{Version: 1, Operation: "release", RequestID: r.RequestID, Status: credentialprovider.StatusReleased}))
	assertions.JSONEq(`{"version":1,"operation":"release","request_id":"55555555-5555-4555-8555-555555555555","status":"released"}`, out.String())
	// Explicitly forbidden predecessor fields are rejected even if zero-valued.
	data = bytes.Replace(data, []byte(`"operation":"release"`), []byte(`"operation":"release","expected_generation":0`), 1)
	_, err = credentialprovider.DecodeRequest(bytes.NewReader(data))
	assertions.ErrorIs(err, credentialprovider.ErrInvalidRequest)
}
