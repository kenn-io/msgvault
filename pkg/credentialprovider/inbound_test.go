package credentialprovider_test

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/credentialprovider"
)

const inboundRequestJSON = `{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111","endpoint":"https://proxy.example.org/gmail/v1","source_type":"gmail","account":"alice@example.com"}`

const inboundReadyJSON = `{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111","status":"ready","endpoint":"https://proxy.example.org/gmail/v1","source_type":"gmail","account":"alice@example.com","credential_id":"44444444-4444-4444-8444-444444444444","credential_generation":1,"issued_at":"2026-10-24T15:00:00Z","expires_at":"2026-10-24T15:05:00Z","credential":{"type":"bearer","token":"synthetic-token"}}`

var inboundScope = credentialprovider.InboundScope{Endpoint: "https://proxy.example.org/gmail/v1", SourceType: "gmail", Account: "alice@example.com"}

func TestInboundRequestScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	r, err := credentialprovider.DecodeInboundRequest(strings.NewReader(inboundRequestJSON))
	requirements.NoError(err)
	assertions.Equal(inboundScope, r.InboundScope)
	for name, input := range map[string]string{
		"loopback http":  strings.Replace(inboundRequestJSON, `https://proxy.example.org`, `http://127.0.0.1:8080`, 1),
		"localhost http": strings.Replace(inboundRequestJSON, `https://proxy.example.org`, `http://localhost:8080`, 1),
		"msmail":         strings.Replace(inboundRequestJSON, `"gmail"`, `"msmail"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := credentialprovider.DecodeInboundRequest(strings.NewReader(input))
			assert.NoError(t, err)
		})
	}
	for name, input := range map[string]string{
		"remote http":                  strings.Replace(inboundRequestJSON, `https:`, `http:`, 1),
		"userinfo":                     strings.Replace(inboundRequestJSON, `proxy.example.org`, `secret@proxy.example.org`, 1),
		"query":                        strings.Replace(inboundRequestJSON, `/gmail/v1`, `/gmail/v1?token=secret`, 1),
		"fragment":                     strings.Replace(inboundRequestJSON, `/gmail/v1`, `/gmail/v1#top`, 1),
		"unknown source type":          strings.Replace(inboundRequestJSON, `"gmail"`, `"imap"`, 1),
		"empty account":                strings.Replace(inboundRequestJSON, `alice@example.com`, ``, 1),
		"account with space":           strings.Replace(inboundRequestJSON, `alice@example.com`, `alice @example.com`, 1),
		"missing account":              strings.Replace(inboundRequestJSON, `,"account":"alice@example.com"`, ``, 1),
		"archive scope":                strings.Replace(inboundRequestJSON, `"version":1`, `"version":1,"archive_id":"archive-a"`, 1),
		"duplicate field":              strings.Replace(inboundRequestJSON, `"version":1`, `"version":1,"version":1`, 1),
		"null field":                   strings.Replace(inboundRequestJSON, `"version":1`, `"version":1,"credential_id":null`, 1),
		"trailing document":            inboundRequestJSON + `{}`,
		"oversize":                     inboundRequestJSON + strings.Repeat(" ", credentialprovider.MaxDocumentBytes),
		"renew without predecessor":    strings.Replace(inboundRequestJSON, `authorize`, `renew`, 1),
		"replace":                      strings.Replace(inboundRequestJSON, `authorize`, `replace`, 1),
		"authorize with predecessor":   strings.Replace(inboundRequestJSON, `"version":1`, `"version":1,"expected_generation":0`, 1),
		"release is not an operation":  strings.Replace(inboundRequestJSON, `authorize`, `release`, 1),
		"noncanonical request id":      strings.Replace(inboundRequestJSON, `11111111-1111-4111-8111-111111111111`, `11111111111141118111111111111111`, 1),
		"mixed-case request id":        strings.Replace(inboundRequestJSON, `11111111-1111-4111-8111-111111111111`, `AAAAAAAA-1111-4111-8111-111111111111`, 1),
		"unsupported protocol version": strings.Replace(inboundRequestJSON, `"version":1`, `"version":2`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := credentialprovider.DecodeInboundRequest(strings.NewReader(input))
			assert.ErrorIs(t, err, credentialprovider.ErrInvalidRequest)
		})
	}
}

func TestInboundResponseMustMatchRequest(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	r, err := credentialprovider.DecodeInboundRequest(strings.NewReader(inboundRequestJSON))
	requirements.NoError(err)
	response, err := credentialprovider.DecodeInboundResponse(strings.NewReader(inboundReadyJSON), r)
	requirements.NoError(err)
	assertions.Equal("synthetic-token", response.Credential.Token)
	assertions.Equal(5*time.Minute, response.ExpiresAt.Sub(response.IssuedAt))
	for name, input := range map[string]string{
		"another endpoint":       strings.Replace(inboundReadyJSON, `/gmail/v1`, `/gmail/v2`, 1),
		"another source type":    strings.Replace(inboundReadyJSON, `"gmail"`, `"msmail"`, 1),
		"another account":        strings.Replace(inboundReadyJSON, `alice@`, `bob@`, 1),
		"another operation":      strings.Replace(inboundReadyJSON, `authorize`, `renew`, 1),
		"another request":        strings.Replace(inboundReadyJSON, `11111111-1111`, `11111112-1111`, 1),
		"missing scope":          strings.Replace(inboundReadyJSON, `"account":"alice@example.com",`, ``, 1),
		"empty token":            strings.Replace(inboundReadyJSON, `synthetic-token`, ``, 1),
		"header injection":       strings.Replace(inboundReadyJSON, `synthetic-token`, `synthetic\r\nHeader:value`, 1),
		"expired interval":       strings.Replace(inboundReadyJSON, `15:05:00`, `14:59:59`, 1),
		"zero generation":        strings.Replace(inboundReadyJSON, `"credential_generation":1`, `"credential_generation":0`, 1),
		"replay status":          strings.Replace(inboundReadyJSON, `"ready"`, `"already_issued"`, 1),
		"refusal with secret":    `{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111","status":"sign_in_required","reason":"no_current_sign_in","credential":{"type":"bearer","token":"synthetic-token"}}`,
		"refusal with scope":     `{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111","status":"sign_in_required","reason":"no_current_sign_in","account":"alice@example.com"}`,
		"refusal without reason": `{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111","status":"unavailable"}`,
		"delegation reason":      `{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111","status":"denied","reason":"delegation_expired"}`,
		"released":               `{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111","status":"released"}`,
	} {
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			_, err := credentialprovider.DecodeInboundResponse(strings.NewReader(input), r)
			requirements.ErrorIs(err, credentialprovider.ErrInvalidResponse)
			assertions.NotContains(err.Error(), "synthetic-token")
		})
	}
	for _, refusal := range []string{
		`"status":"sign_in_required","reason":"no_current_sign_in"`,
		`"status":"approval_required","reason":"account_approval_required"`,
		`"status":"conflict","reason":"generation_changed"`,
		`"status":"unavailable","reason":"authority_unavailable"`,
	} {
		response, err := credentialprovider.DecodeInboundResponse(strings.NewReader(`{"version":1,"operation":"authorize","request_id":"11111111-1111-4111-8111-111111111111",`+refusal+`}`), r)
		requirements.NoError(err)
		assertions.Nil(response.Credential)
	}
}

func TestInboundRenewalContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	r, err := credentialprovider.DecodeInboundRequest(strings.NewReader(inboundRequestJSON))
	requirements.NoError(err)
	ready, err := credentialprovider.DecodeInboundResponse(strings.NewReader(inboundReadyJSON), r)
	requirements.NoError(err)
	r.Operation = "renew"
	r.PreviousRequestID = r.RequestID
	r.RequestID = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	r.CredentialID = ready.CredentialID
	r.ExpectedGeneration = 1
	data, err := json.Marshal(r)
	requirements.NoError(err)
	_, err = credentialprovider.DecodeInboundRequest(bytes.NewReader(data))
	requirements.NoError(err)
	ready.Operation, ready.RequestID = r.Operation, r.RequestID
	var out bytes.Buffer
	ready.CredentialGeneration = 2
	requirements.ErrorIs(credentialprovider.WriteInboundResponse(&out, r, ready), credentialprovider.ErrInvalidResponse)
	assertions.Empty(out.Bytes())
	ready.CredentialID = uuid.MustParse("66666666-6666-4666-8666-666666666666")
	ready.CredentialGeneration = 1
	requirements.ErrorIs(credentialprovider.WriteInboundResponse(&out, r, ready), credentialprovider.ErrInvalidResponse)
	ready.CredentialGeneration = 2
	requirements.NoError(credentialprovider.WriteInboundResponse(&out, r, ready))
	decoded, err := credentialprovider.DecodeInboundResponse(&out, r)
	requirements.NoError(err)
	assertions.Equal(int64(2), decoded.CredentialGeneration)
}

func TestExchangeInboundHelper(t *testing.T) {
	requirements := require.New(t)
	executable, err := os.Executable()
	requirements.NoError(err)
	r, err := credentialprovider.DecodeInboundRequest(strings.NewReader(inboundRequestJSON))
	requirements.NoError(err)
	for _, tc := range []struct {
		mode string
		want error
	}{
		{"ready", nil},
		{"refuse", nil},
		{"wrong-scope", credentialprovider.ErrInvalidResponse},
		{"nonzero", credentialprovider.ErrProviderFailed},
		{"invalid-request", credentialprovider.ErrInvalidRequest},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			response, err := credentialprovider.ExchangeInbound(t.Context(), inboundHelper(t, executable, tc.mode, time.Hour), r)
			if tc.want != nil {
				requirements.ErrorIs(err, tc.want)
				assertions.Nil(response.Credential)
				assertions.NotContains(err.Error(), "synthetic-token")
				return
			}
			requirements.NoError(err)
			if tc.mode == "ready" {
				requirements.NotNil(response.Credential)
				assertions.Equal("synthetic-token-1", response.Credential.Token)
			} else {
				assertions.Equal(credentialprovider.StatusSignInRequired, response.Status)
			}
		})
	}
}

func TestInboundSourceReusesCredentialUntilExpiry(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	executable, err := os.Executable()
	requirements.NoError(err)
	argv := inboundHelper(t, executable, "ready", time.Hour)
	source, err := credentialprovider.NewInboundSource(argv, inboundScope)
	requirements.NoError(err)
	var wg sync.WaitGroup
	tokens := make([]credentialprovider.InboundToken, 8)
	errs := make([]error, len(tokens))
	for i := range tokens {
		wg.Go(func() { tokens[i], errs[i] = source.Token(t.Context()) })
	}
	wg.Wait()
	for i := range tokens {
		requirements.NoError(errs[i])
		assertions.Equal("synthetic-token-1", tokens[i].Bearer)
	}
	assertions.Len(helperRequests(t, argv), 1)

	argv = inboundHelper(t, executable, "ready", time.Nanosecond)
	source, err = credentialprovider.NewInboundSource(argv, inboundScope)
	requirements.NoError(err)
	first, err := source.Token(t.Context())
	requirements.NoError(err)
	second, err := source.Token(t.Context())
	requirements.NoError(err)
	assertions.Equal("synthetic-token-1", first.Bearer)
	assertions.Equal("synthetic-token-2", second.Bearer)
	requests := helperRequests(t, argv)
	requirements.Len(requests, 2)
	assertions.Equal("authorize", requests[0].Operation)
	assertions.Equal("renew", requests[1].Operation)
	assertions.Equal(requests[0].RequestID, requests[1].PreviousRequestID)
	assertions.Equal(int64(1), requests[1].ExpectedGeneration)
}

func TestInboundSourceRenewsRejectedCredentialOnce(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	executable, err := os.Executable()
	requirements.NoError(err)
	argv := inboundHelper(t, executable, "ready", time.Hour)
	source, err := credentialprovider.NewInboundSource(argv, inboundScope)
	requirements.NoError(err)
	rejected, err := source.Token(t.Context())
	requirements.NoError(err)
	renewed, err := source.Renew(t.Context(), rejected)
	requirements.NoError(err)
	again, err := source.Renew(t.Context(), rejected)
	requirements.NoError(err)
	assertions.Equal("synthetic-token-2", renewed.Bearer)
	assertions.Equal(renewed, again)
	requests := helperRequests(t, argv)
	requirements.Len(requests, 2)
	assertions.Equal("renew", requests[1].Operation)
}

func TestInboundSourceAuthorizesAfterRefusal(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	executable, err := os.Executable()
	requirements.NoError(err)
	argv := inboundHelper(t, executable, "refuse", time.Hour)
	source, err := credentialprovider.NewInboundSource(argv, inboundScope)
	requirements.NoError(err)
	for range 2 {
		_, err := source.Token(t.Context())
		refusal, ok := errors.AsType[*credentialprovider.RefusalError](err)
		requirements.True(ok, "expected a refusal, got %v", err)
		assertions.Equal(credentialprovider.StatusSignInRequired, refusal.Status)
	}
	for _, request := range helperRequests(t, argv) {
		assertions.Equal("authorize", request.Operation)
	}
	_, err = credentialprovider.NewInboundSource(argv, credentialprovider.InboundScope{Endpoint: "http://proxy.example.org", SourceType: "gmail", Account: "alice@example.com"})
	assertions.ErrorIs(err, credentialprovider.ErrInvalidRequest)
}

func inboundHelper(t *testing.T, executable, mode string, lifetime time.Duration) []string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "requests.jsonl")
	return []string{executable, "-test.run=^TestInboundProviderProcess$", "--", mode, log, lifetime.String()}
}

func helperRequests(t *testing.T, argv []string) []credentialprovider.InboundRequest {
	t.Helper()
	file, err := os.Open(argv[len(argv)-2])
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	var requests []credentialprovider.InboundRequest
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		request, err := credentialprovider.DecodeInboundRequest(bytes.NewReader(scanner.Bytes()))
		require.NoError(t, err)
		requests = append(requests, request)
	}
	require.NoError(t, scanner.Err())
	return requests
}

// This is the executable peer for ExchangeInbound and InboundSource. It records
// each request it reads and issues the next generation of the credential.
func TestInboundProviderProcess(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 4 {
		return
	}
	mode, logPath := args[1], args[2]
	lifetime, err := time.ParseDuration(args[3])
	if err != nil {
		os.Exit(1)
	}
	r, err := credentialprovider.DecodeInboundRequest(os.Stdin)
	if err != nil || mode == "invalid-request" {
		os.Exit(2)
	}
	data, err := json.Marshal(r)
	if err != nil {
		os.Exit(1)
	}
	log, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- the parent test chose this temporary path.
	if err != nil {
		os.Exit(1)
	}
	_, err = log.Write(append(data, '\n'))
	if err != nil || log.Close() != nil {
		os.Exit(1)
	}
	if mode == "refuse" {
		err := credentialprovider.WriteInboundResponse(os.Stdout, r, credentialprovider.InboundResponse{Version: r.Version, Operation: r.Operation, RequestID: r.RequestID, Status: credentialprovider.StatusSignInRequired, Reason: "no_current_sign_in"})
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	issued := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)
	generation := r.ExpectedGeneration + 1
	response := credentialprovider.InboundResponse{
		InboundScope: r.InboundScope, Version: r.Version, Operation: r.Operation, RequestID: r.RequestID,
		Status: credentialprovider.StatusReady, CredentialID: uuid.New(), CredentialGeneration: generation,
		IssuedAt: issued, ExpiresAt: issued.Add(lifetime),
		Credential: &credentialprovider.Credential{Type: "bearer", Token: "synthetic-token-" + strconv.FormatInt(generation, 10)},
	}
	if mode == "wrong-scope" {
		response.Account = "bob@example.com"
		data, err := json.Marshal(response)
		if err != nil {
			os.Exit(1)
		}
		_, _ = os.Stdout.Write(data)
		os.Exit(0)
	}
	if err := credentialprovider.WriteInboundResponse(os.Stdout, r, response); err != nil {
		os.Exit(1)
	}
	_, _ = os.Stderr.WriteString("synthetic-token must never reach diagnostics")
	if mode == "nonzero" {
		os.Exit(1)
	}
	os.Exit(0)
}
