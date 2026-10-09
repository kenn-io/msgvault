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
	"go.kenn.io/msgvault/internal/muesli"
)

type fakeMuesliImportStore struct {
	*mockStore

	calls int
}

func (s *fakeMuesliImportStore) ImportMuesli(_ context.Context, _ muesli.RemoteRequest) (muesli.RemoteResult, error) {
	s.calls++
	return muesli.RemoteResult{Status: "registered", SourceID: 1}, nil
}
func TestMuesliImportBoundary(t *testing.T) {
	for _, tt := range []struct {
		name, body, key, media string
		status                 int
	}{
		{"owner", `{"action":"register","source":{"identifier":"recorder","account_email":"user@example.com"}}`, meetingImportTestAPIKey, "application/json", 200},
		{"unauthorized", `{}`, "", "application/json", 401},
		{"validation", `{"action":"register","source":{"identifier":"recorder","account_email":"invalid"}}`, meetingImportTestAPIKey, "application/json", 422},
		{"unknown field", `{"action":"register","secret":"private"}`, meetingImportTestAPIKey, "application/json", 400},
		{"duplicate field", `{"action":"register","action":"upsert"}`, meetingImportTestAPIKey, "application/json", 400},
		{"media", `{}`, meetingImportTestAPIKey, "text/plain", 415},
		{"oversized", strings.Repeat(" ", int(muesli.MaxRemoteRequestBytes)+1), meetingImportTestAPIKey, "application/json", 413},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			st := &fakeMuesliImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
			server := NewServer(&config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey}}, st, nil, testLogger())
			req := httptest.NewRequest(http.MethodPost, "/api/v1/import/muesli", strings.NewReader(tt.body))
			req.Header.Set("X-Api-Key", tt.key)
			req.Header.Set("Content-Type", tt.media)
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

func TestMuesliImportRejectsDelegatedToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := &fakeMuesliImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
	server := NewServer(&config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	registry := agentgrant.NewRegistry()
	server.agentGrants = registry
	_, secret, _, err := registry.Issue("synthetic agent", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "user@example.com"}})
	require.NoError(err)
	health := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	health.Header.Set(apiprotocol.AgentTokenHeader, secret)
	healthOut := httptest.NewRecorder()
	server.Router().ServeHTTP(healthOut, health)
	require.Equal(http.StatusOK, healthOut.Code, "delegated token must authenticate before endpoint denial")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import/muesli", strings.NewReader(`{"action":"register","source":{"identifier":"recorder","account_email":"user@example.com"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiprotocol.AgentTokenHeader, secret)
	out := httptest.NewRecorder()
	server.Router().ServeHTTP(out, req)
	assert.Equal(http.StatusUnauthorized, out.Code)
	assert.Zero(st.calls)
}

func TestMuesliImportWaitsForOperationGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := &fakeMuesliImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
	gate := NewSerialOperationGate()
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey}}, Store: st, Logger: testLogger(), OperationGate: gate})
	release, ok := gate.BeginWork()
	require.True(ok)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import/muesli", strings.NewReader(`{"action":"register","source":{"identifier":"recorder","account_email":"user@example.com"}}`))
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

func TestMuesliContactReviewValidationBeforeGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := &fakeMuesliImportStore{mockStore: &mockStore{stats: &StoreStats{}}}
	gate := NewSerialOperationGate()
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey}}, Store: st, Logger: testLogger(), OperationGate: gate})
	release, ok := gate.BeginWork()
	require.True(ok)
	defer release()
	body := `{"action":"upsert","source":{"identifier":"recorder","account_email":"user@example.com"},"meeting":{"contacts_state":"complete","record":{"id":42,"title":"Planning","created_at":"2026-09-01T14:00:03Z","start_time":"2026-09-01T14:00:00Z","status":"completed","raw_transcript":"Synthetic transcript"},"participants":[{"email":"attendee@example.com","resolution":"resolved","contact_review_phones":["+16045550100"]}]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/import/muesli", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", meetingImportTestAPIKey)
	out := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { server.Router().ServeHTTP(out, req); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow("invalid contact evidence waited for operation gate")
	}
	assert.Equal(http.StatusUnprocessableEntity, out.Code)
	assert.Zero(st.calls)
	assert.False(gate.HasRequestWaiters())
	assert.NotContains(out.Body.String(), "attendee@example.com")
	assert.NotContains(out.Body.String(), "+16045550100")
}
