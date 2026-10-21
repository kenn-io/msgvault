package api

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

type callHTTPStore struct {
	*mockStore
	CallHistoryStore
}

func TestCallHistoryHTTPRoundtripAndAuthorization(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	archive := testutil.NewTestStore(t)
	peer, err := archive.EnsureParticipantByPhone("+12025550101", "Example", "signal")
	requirements.NoError(err)
	person, _, err := archive.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	st := &callHTTPStore{mockStore: &mockStore{stats: &StoreStats{}}, CallHistoryStore: archive}
	server := NewServer(&config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	request := callhistory.ImportRequest{
		Format: "signal_desktop", Account: "example",
		Record:   []byte(`{"callId":"one","peerId":"peer","mode":"Direct","status":"Accepted","timestamp":1790841600000}`),
		Bindings: map[string]int64{"peer": peer},
	}
	body, err := json.Marshal(request)
	requirements.NoError(err)
	do := func(method, path, body, key, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("X-Api-Key", key)
		}
		if token != "" {
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
		}
		out := httptest.NewRecorder()
		server.Router().ServeHTTP(out, req)
		return out
	}
	for range 2 {
		out := do("POST", "/api/v1/import/calls", string(body), meetingImportTestAPIKey, "")
		requirements.Equal(200, out.Code, out.Body.String())
	}
	out := do("GET", "/api/v1/calls/summary", "", meetingImportTestAPIKey, "")
	requirements.Equal(200, out.Code, out.Body.String())
	var summary CallSummaryResponse
	requirements.NoError(json.Unmarshal(out.Body.Bytes(), &summary))
	requirements.Len(summary.Items, 1)
	assertions.Equal(person.ID, summary.Items[0].PersonID)
	assertions.Equal(1, summary.Items[0].Answered)
	out = do("GET", "/api/v1/calls", "", meetingImportTestAPIKey, "")
	requirements.Equal(200, out.Code)
	var evidence CallEvidenceResponse
	requirements.NoError(json.Unmarshal(out.Body.Bytes(), &evidence))
	requirements.Len(evidence.Items, 1)
	assertions.Equal("joined", evidence.Items[0].Participants[0].Participation)
	assertions.NotContains(out.Body.String(), `"raw"`)
	request.Bindings["peer"] = 9223372036854775807
	invalidBody, err := json.Marshal(request)
	requirements.NoError(err)
	assertions.Equal(422, do("POST", "/api/v1/import/calls", string(invalidBody), meetingImportTestAPIKey, "").Code)
	unchanged := do("GET", "/api/v1/calls", "", meetingImportTestAPIKey, "")
	assertions.JSONEq(out.Body.String(), unchanged.Body.String())
	for _, path := range []string{"/api/v1/calls?offset=1", "/api/v1/calls/summary?after_id=1"} {
		assertions.Equal(400, do("GET", path, "", meetingImportTestAPIKey, "").Code)
	}
	for _, path := range []string{"/api/v1/calls?after_id=9223372036854775807", "/api/v1/calls/summary?offset=1"} {
		page := do("GET", path, "", meetingImportTestAPIKey, "")
		requirements.Equal(200, page.Code)
		assertions.JSONEq(`{"items":[]}`, page.Body.String())
	}
	registry := agentgrant.NewRegistry()
	server.agentGrants = registry
	_, token, _, err := registry.Issue("synthetic agent", []agentgrant.Permission{agentgrant.PermissionDraftCreate},
		[]agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "example@example.com"}}, time.Time{})
	requirements.NoError(err)
	requirements.Equal(200, do("GET", "/api/v1/health", "", "", token).Code)
	for _, endpoint := range []struct{ method, path string }{
		{"POST", "/api/v1/import/calls"}, {"GET", "/api/v1/calls"}, {"GET", "/api/v1/calls/summary"},
	} {
		assertions.Equal(401, do(endpoint.method, endpoint.path, string(body), "", "").Code)
		assertions.Equal(401, do(endpoint.method, endpoint.path, string(body), "", token).Code)
	}
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/api/v1/import/calls", "{", 400},
		{"/api/v1/import/calls", `{}`, 422},
		{"/api/v1/import/calls", strings.Repeat(" ", callhistory.MaxRequestBytes+1), 413},
	} {
		assertions.Equal(tc.status, do("POST", tc.path, tc.body, meetingImportTestAPIKey, "").Code)
	}
	assertions.Equal(400, do("GET", "/api/v1/calls?limit=101", "", meetingImportTestAPIKey, "").Code)
	assertions.Equal(400, do("GET", "/api/v1/calls/summary?after=invalid", "", meetingImportTestAPIKey, "").Code)
}

func TestCallHistoryImportUsesMutationGate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	archive := testutil.NewTestStore(t)
	st := &callHTTPStore{mockStore: &mockStore{stats: &StoreStats{}}, CallHistoryStore: archive}
	gate := NewSerialOperationGate()
	server := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey}},
		Store:  st, Logger: testLogger(), OperationGate: gate,
	})
	release, ok := gate.BeginWork()
	requirements.True(ok)
	defer release()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import/calls", strings.NewReader(
		`{"format":"signal_desktop","account":"example","record":{"callId":"x","timestamp":0}}`))
	req.Header.Set("X-Api-Key", meetingImportTestAPIKey)
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { server.Router().ServeHTTP(out, req); close(done) }()
	requirements.Eventually(gate.HasRequestWaiters, 5*time.Second, 10*time.Millisecond)
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		requirements.FailNow("import did not finish after mutation gate release")
	}
	assertions.Equal(200, out.Code, out.Body.String())
}
