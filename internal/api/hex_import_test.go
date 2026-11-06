package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/testutil"
)

type fakeHexImportStore struct {
	*mockStore

	calls int
	run   func(context.Context, hex.RemoteRequest) (hex.RemoteResult, error)
}

func (s *fakeHexImportStore) ImportHex(ctx context.Context, req hex.RemoteRequest) (hex.RemoteResult, error) {
	s.calls++
	if s.run != nil {
		return s.run(ctx, req)
	}
	return hex.RemoteResult{Status: "registered", SourceID: 1}, nil
}
func TestHexImportBoundary(t *testing.T) {
	for _, tt := range []struct {
		name, body, key, media string
		status                 int
		delegated              bool
	}{
		{"owner", `{"action":"register","source":{"identifier":"recorder","account_email":"user@example.com"}}`, meetingImportTestAPIKey, "application/json", 200, false},
		{"delegated", `{"action":"register","source":{"identifier":"recorder","account_email":"user@example.com"}}`, "", "application/json", 401, true},
		{"unauthorized", `{}`, "", "application/json", 401, false},
		{"validation", `{"action":"register","source":{"identifier":"recorder","account_email":"invalid"}}`, meetingImportTestAPIKey, "application/json", 422, false},
		{"unknown field", `{"action":"register","secret":"private"}`, meetingImportTestAPIKey, "application/json", 400, false},
		{"duplicate field", `{"action":"register","action":"upsert"}`, meetingImportTestAPIKey, "application/json", 400, false},
		{"media", `{}`, meetingImportTestAPIKey, "text/plain", 415, false},
		{"oversized", strings.Repeat(" ", int(hex.MaxRemoteRequestBytes)+1), meetingImportTestAPIKey, "application/json", 413, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			st := &fakeHexImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
			server := NewServer(&config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey, AgentAccess: tt.delegated}}, st, nil, testLogger())

			var secret string
			if tt.delegated {
				registry := agentgrant.NewRegistry()
				server.agentGrants = registry
				_, token, _, err := registry.Issue("synthetic agent", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "user@example.com"}}, time.Time{})
				require.NoError(t, err)
				secret = token
				health := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
				health.Header.Set(apiprotocol.AgentTokenHeader, secret)
				healthOut := httptest.NewRecorder()
				server.Router().ServeHTTP(healthOut, health)
				require.Equal(t, http.StatusOK, healthOut.Code, "delegated token authenticates before endpoint denial")
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/import/hex", strings.NewReader(tt.body))
			req.Header.Set("X-Api-Key", tt.key)
			req.Header.Set("Content-Type", tt.media)
			if tt.delegated {
				req.Header.Set(apiprotocol.AgentTokenHeader, secret)
			}
			out := httptest.NewRecorder()
			server.Router().ServeHTTP(out, req)
			assert.Equal(tt.status, out.Code)
			if tt.status != 200 {
				assert.Zero(st.calls)
				assert.NotContains(out.Body.String(), "private")
			} else {
				assert.Equal(1, st.calls)
			}
		})
	}
}

func TestHexImportRejectsEmptyEntryTextAndAcceptsEmptyFinalTranscript(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	archive := testutil.NewTestStore(t)
	importer := hex.NewImporter(archive)
	source := meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}
	_, err := importer.ImportRemote(t.Context(), hex.RemoteRequest{Action: "register", Source: source})
	must.NoError(err)
	meeting := hex.Meeting{
		Manifest:        hex.Manifest{SchemaVersion: 1, ID: "http-transcript", Title: "Synthetic call", Status: "complete", CreatedAtMS: 1790000000123},
		Entries:         []hex.Entry{{Source: "system", StartMS: 0, EndMS: 1000, Text: "Readable transcript"}},
		TranscriptState: "final",
	}
	seed, err := importer.ImportRemote(t.Context(), hex.RemoteRequest{Action: "upsert", Source: source, Meeting: &meeting, Full: true})
	must.NoError(err)
	before, err := archive.GetMessage(seed.MessageID)
	must.NoError(err)
	beforeRaw, err := archive.GetMessageRaw(seed.MessageID)
	must.NoError(err)

	apiStore := &fakeHexImportStore{
		mockStore: &mockStore{stats: &StoreStats{}},
		run:       importer.ImportRemote,
	}
	server := NewServer(&config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey}}, apiStore, nil, testLogger())
	const sourceJSON = `{"identifier":"recorder","account_email":"user@example.com"}`
	const manifestJSON = `{"schema_version":1,"id":"http-transcript","title":"Synthetic call","status":"complete","created_at_ms":1790000000123}`
	requestBody := func(entries string) string {
		return `{"action":"upsert","full":true,"source":` + sourceJSON + `,"meeting":{"manifest":` + manifestJSON + `,"entries":` + entries + `,"transcript_state":"final"}}`
	}
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/import/hex", strings.NewReader(body))
		req.Header.Set("X-Api-Key", meetingImportTestAPIKey)
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		server.Router().ServeHTTP(out, req)
		return out
	}
	for _, entry := range []struct {
		name  string
		value string
	}{
		{"omitted", `{"source":"system","start_ms":0,"end_ms":1000}`},
		{"null", `{"source":"system","start_ms":0,"end_ms":1000,"text":null}`},
		{"blank", `{"source":"system","start_ms":0,"end_ms":1000,"text":" \t "}`},
	} {
		t.Run(entry.name, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			apiStore.calls = 0
			out := post(requestBody("[" + entry.value + "]"))
			check.Equal(http.StatusUnprocessableEntity, out.Code)
			check.Zero(apiStore.calls, "invalid transcript entries must not reach the importer")
			after, err := archive.GetMessage(seed.MessageID)
			must.NoError(err)
			check.Equal(before.Body, after.Body)
			afterRaw, err := archive.GetMessageRaw(seed.MessageID)
			must.NoError(err)
			check.Equal(beforeRaw, afterRaw)
		})
	}

	apiStore.calls = 0
	accepted := post(requestBody("[]"))
	check.Equal(http.StatusOK, accepted.Code, accepted.Body.String())
	check.Equal(1, apiStore.calls)
	after, err := archive.GetMessage(seed.MessageID)
	must.NoError(err)
	check.NotContains(after.Body, "Readable transcript", "an explicit empty final transcript must be accepted")
}

func TestHexImportWaitsForOperationGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := &fakeHexImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
	gate := NewSerialOperationGate()
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey}}, Store: st, Logger: testLogger(), OperationGate: gate})
	release, ok := gate.BeginWork()
	require.True(ok)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import/hex", strings.NewReader(`{"action":"register","source":{"identifier":"recorder","account_email":"user@example.com"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", meetingImportTestAPIKey)
	out := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { server.Router().ServeHTTP(out, req); close(done) }()
	require.Eventually(gate.HasRequestWaiters, 5*time.Second, 10*time.Millisecond)
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow("request did not finish after gate release")
	}
	assert.Equal(http.StatusOK, out.Code)
	assert.Equal(1, st.calls)
}
