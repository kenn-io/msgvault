package remotecredential_test

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/remotecredential"
	"go.kenn.io/msgvault/pkg/credentialprovider"
)

var testScope = credentialprovider.Scope{
	Endpoint:     "https://archive.example.org/messages",
	ArchiveID:    "archive-a",
	ClientID:     uuid.MustParse("22222222-2222-4222-8222-222222222222"),
	DelegationID: uuid.MustParse("33333333-3333-4333-8333-333333333333"),
	SourceIDs:    []string{"17", "23"},
	Operations:   []string{"getHealth", "searchCLI"},
}

// helper returns argv that runs this test binary as the credential helper,
// and the directory holding the helper's records.
func helper(t *testing.T) ([]string, string) {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	state := t.TempDir()
	return []string{executable, "-test.run=^TestHelperProcess$", "--", state}, state
}

func setHelper(t *testing.T, state, name, value string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(state, name), []byte(value), 0o600))
}

func helperRequests(t *testing.T, state string) []credentialprovider.Request {
	t.Helper()
	file, err := os.Open(filepath.Join(state, "requests.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	var requests []credentialprovider.Request
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		request, err := credentialprovider.DecodeRequest(bytes.NewReader(scanner.Bytes()))
		require.NoError(t, err)
		requests = append(requests, request)
	}
	require.NoError(t, scanner.Err())
	return requests
}

func TestSourceReplacesAcrossCommandsAndRenewsBeforeExpiry(t *testing.T) {
	synctest.Test(t, testSourceReplacesAcrossCommandsAndRenewsBeforeExpiry)
}

func testSourceReplacesAcrossCommandsAndRenewsBeforeExpiry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	argv, state := helper(t)
	journals := t.TempDir()

	first, err := remotecredential.New(argv, testScope, journals)
	require.NoError(err)
	bearer, err := first.Token(t.Context())
	require.NoError(err)
	assert.Equal("synthetic-bearer-1", bearer)
	again, err := first.Token(t.Context())
	require.NoError(err)
	assert.Equal(bearer, again, "a usable bearer is reused without running the helper")

	// A later command has no bearer in memory, so it replaces the one the
	// first command received.
	second, err := remotecredential.New(argv, testScope, journals)
	require.NoError(err)
	setHelper(t, state, "lifetime", "2s")
	bearer, err = second.Token(t.Context())
	require.NoError(err)
	assert.Equal("synthetic-bearer-2", bearer)

	// A two-second bearer is usable for one second; then it is renewed.
	time.Sleep(1100 * time.Millisecond)
	bearer, err = second.Token(t.Context())
	require.NoError(err)
	assert.Equal("synthetic-bearer-3", bearer)

	requests := helperRequests(t, state)
	require.Len(requests, 3)
	assert.Equal([]string{"authorize", "replace", "renew"}, []string{requests[0].Operation, requests[1].Operation, requests[2].Operation})
	for i, request := range requests {
		assert.Equal(testScope, request.Scope)
		if i > 0 {
			assert.Equal(requests[i-1].RequestID, request.PreviousRequestID, "each request names its predecessor")
			assert.Equal(int64(i), request.ExpectedGeneration)
		}
	}
	journal, err := os.ReadFile(filepath.Join(journals, testScope.DelegationID.String()+".json"))
	require.NoError(err)
	assert.NotContains(string(journal), "synthetic-bearer", "the journal never holds a credential")
}

func TestSourceReplaysAnInterruptedExchange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	argv, state := helper(t)
	journals := t.TempDir()
	setHelper(t, state, "mode", "exit-after-commit")

	source, err := remotecredential.New(argv, testScope, journals)
	require.NoError(err)
	_, err = source.Token(t.Context())
	require.ErrorIs(err, credentialprovider.ErrProviderFailed)

	// The next command replays the saved request. The helper reports that it
	// already issued that credential, so the command replaces it.
	source, err = remotecredential.New(argv, testScope, journals)
	require.NoError(err)
	bearer, err := source.Token(t.Context())
	require.NoError(err)
	assert.Equal("synthetic-bearer-2", bearer)

	requests := helperRequests(t, state)
	require.Len(requests, 3)
	assert.Equal(requests[0], requests[1], "the interrupted request is replayed exactly")
	assert.Equal("replace", requests[2].Operation)
	assert.Equal(requests[0].RequestID, requests[2].PreviousRequestID)
	assert.Equal(int64(1), requests[2].ExpectedGeneration)
}

func TestSourceReportsRefusalsWithoutReplayingThem(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	argv, state := helper(t)
	journals := t.TempDir()
	setHelper(t, state, "mode", "sign-in")

	source, err := remotecredential.New(argv, testScope, journals)
	require.NoError(err)
	_, err = source.Token(t.Context())
	refusal, ok := errors.AsType[*credentialprovider.RefusalError](err)
	require.True(ok, "%v", err)
	assert.Equal(credentialprovider.StatusSignInRequired, refusal.Status)

	require.NoError(os.Remove(filepath.Join(state, "mode")))
	bearer, err := source.Token(t.Context())
	require.NoError(err)
	assert.Equal("synthetic-bearer-1", bearer)
	requests := helperRequests(t, state)
	require.Len(requests, 2)
	assert.NotEqual(requests[0].RequestID, requests[1].RequestID, "a refused request is decided, not replayed")
	assert.Equal("authorize", requests[1].Operation)
}

func TestSourceStartsOverAfterALineageConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	argv, state := helper(t)
	journals := t.TempDir()

	source, err := remotecredential.New(argv, testScope, journals)
	require.NoError(err)
	_, err = source.Token(t.Context())
	require.NoError(err)
	// Another client of the helper moved the lineage on.
	setHelper(t, state, "generation", "5")

	source, err = remotecredential.New(argv, testScope, journals)
	require.NoError(err)
	_, err = source.Token(t.Context())
	refusal, ok := errors.AsType[*credentialprovider.RefusalError](err)
	require.True(ok, "%v", err)
	assert.Equal(credentialprovider.StatusConflict, refusal.Status)

	bearer, err := source.Token(t.Context())
	require.NoError(err)
	assert.Equal("synthetic-bearer-6", bearer)
	requests := helperRequests(t, state)
	require.Len(requests, 3)
	assert.Equal("authorize", requests[2].Operation, "the next request starts a new lineage instead of guessing a generation")
}

func TestNewRejectsAScopeTheHelperCannotAccept(t *testing.T) {
	scope := testScope
	scope.Endpoint = "http://archive.example.org/messages"
	_, err := remotecredential.New([]string{"helper"}, scope, t.TempDir())
	require.ErrorContains(t, err, "https url")
	_, err = remotecredential.New(nil, testScope, t.TempDir())
	require.ErrorContains(t, err, "needs a command")
}

type helperState struct {
	Generation int64                    `json:"generation"`
	Issued     map[uuid.UUID]issuedCred `json:"issued"`
}

type issuedCred struct {
	CredentialID uuid.UUID `json:"credential_id"`
	Generation   int64     `json:"generation"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// TestHelperProcess is the credential helper the tests run. It replays a
// request it has seen without the secret, refuses a predecessor that is not the
// newest credential, and otherwise issues the next generation.
func TestHelperProcess(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 2 {
		return
	}
	dir := args[1]
	read := func(name string) string {
		data, _ := os.ReadFile(filepath.Join(dir, name))
		return string(data)
	}
	request, err := credentialprovider.DecodeRequest(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	line, err := json.Marshal(request)
	if err != nil {
		os.Exit(1)
	}
	log, err := os.OpenFile(filepath.Join(dir, "requests.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(1)
	}
	if _, err := log.Write(append(line, '\n')); err != nil || log.Close() != nil {
		os.Exit(1)
	}
	envelope := credentialprovider.Response{Version: request.Version, Operation: request.Operation, RequestID: request.RequestID}
	respond := func(response credentialprovider.Response) {
		if credentialprovider.WriteResponse(os.Stdout, request, response) != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	mode := read("mode")
	if mode == "sign-in" {
		envelope.Status, envelope.Reason = credentialprovider.StatusSignInRequired, "no_current_sign_in"
		respond(envelope)
	}
	state := helperState{Issued: map[uuid.UUID]issuedCred{}}
	if data := read("state.json"); data != "" && json.Unmarshal([]byte(data), &state) != nil {
		os.Exit(1)
	}
	save := func() {
		data, err := json.Marshal(state)
		if err != nil || os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600) != nil {
			os.Exit(1)
		}
	}
	if raw := read("generation"); raw != "" {
		state.Generation, _ = strconv.ParseInt(raw, 10, 64)
		save()
		_ = os.Remove(filepath.Join(dir, "generation"))
	}
	issued := func(status credentialprovider.Status, cred issuedCred) credentialprovider.Response {
		response := envelope
		response.Scope = request.Scope
		response.Status = status
		response.CredentialID, response.CredentialGeneration = cred.CredentialID, cred.Generation
		response.IssuedAt, response.ExpiresAt = cred.IssuedAt, cred.ExpiresAt
		return response
	}
	if prior, ok := state.Issued[request.RequestID]; ok {
		status := credentialprovider.StatusAlreadyIssued
		if request.Operation != "authorize" {
			status = credentialprovider.StatusReplacementRequired
		}
		respond(issued(status, prior))
	}
	if request.Operation != "authorize" && request.ExpectedGeneration != state.Generation {
		envelope.Status, envelope.Reason = credentialprovider.StatusConflict, "generation_changed"
		respond(envelope)
	}
	lifetime := 15 * time.Minute
	if raw := read("lifetime"); raw != "" {
		lifetime, _ = time.ParseDuration(raw)
	}
	now := time.Now().UTC()
	state.Generation++
	cred := issuedCred{CredentialID: uuid.New(), Generation: state.Generation, IssuedAt: now, ExpiresAt: now.Add(lifetime)}
	state.Issued[request.RequestID] = cred
	save()
	if mode == "exit-after-commit" {
		_ = os.Remove(filepath.Join(dir, "mode"))
		os.Exit(1)
	}
	response := issued(credentialprovider.StatusReady, cred)
	response.Credential = &credentialprovider.Credential{Type: "bearer", Token: "synthetic-bearer-" + strconv.FormatInt(cred.Generation, 10)}
	respond(response)
}
