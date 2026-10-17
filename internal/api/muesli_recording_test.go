package api

import (
	"context"
	"io"
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

type recordingBoundaryStore struct {
	*mockStore

	calls int
	cap   int64
}

func (s *recordingBoundaryStore) ImportMuesliRecording(_ context.Context, _ muesli.RecordingUpload, body io.Reader, capBytes int64) (bool, error) {
	s.calls++
	s.cap = capBytes
	_, err := io.Copy(io.Discard, body)
	return true, err
}

func TestMuesliRecordingOwnerBoundaryAndSizeCap(t *testing.T) {
	for _, tt := range []struct {
		name, key, size, media string
		cap                    int64
		status                 int
	}{
		{"owner", meetingImportTestAPIKey, "12", "audio/wav", 0, 200},
		{"m4a", meetingImportTestAPIKey, "12", "audio/mp4", 0, 200},
		{"unauthenticated", "", "12", "audio/wav", 0, 401},
		{"over cap", meetingImportTestAPIKey, "13", "audio/wav", 12, 413},
		{"default cap", meetingImportTestAPIKey, "4294967297", "audio/wav", 0, 413},
		{"invalid size", meetingImportTestAPIKey, "-1", "audio/wav", 0, 422},
		{"unsupported media", meetingImportTestAPIKey, "12", "application/octet-stream", 0, 415},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)

			st := &recordingBoundaryStore{mockStore: &mockStore{stats: &StoreStats{}}}
			cfg := &config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey, MuesliRecordingMaxBytes: tt.cap}}
			server := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
			url := muesliRecordingEndpointPath + "?identifier=recorder&account_email=user%40example.com&message_id=42&sha256=" + strings.Repeat("a", 64) + "&size=" + tt.size
			req := httptest.NewRequest(http.MethodPost, url, strings.NewReader("twelve bytes"))
			req.Header.Set("Content-Type", tt.media)
			req.Header.Set("X-Api-Key", tt.key)
			out := httptest.NewRecorder()
			server.Router().ServeHTTP(out, req)
			assert.Equal(tt.status, out.Code, out.Body.String())
			if tt.status == 200 {
				assert.Equal(int64(4)<<30, st.cap)
				assert.Equal(1, st.calls)
			} else {
				assert.Zero(st.calls)
			}
		})
	}
}

func TestMuesliRecordingRejectsDelegatedToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := &recordingBoundaryStore{mockStore: &mockStore{stats: &StoreStats{}}}
	server := NewServer(&config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	registry := agentgrant.NewRegistry()
	server.agentGrants = registry
	_, secret, _, err := registry.Issue("synthetic agent", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "muesli", Identifier: "recorder"}}, time.Time{})
	require.NoError(err)
	req := httptest.NewRequest(http.MethodPost, muesliRecordingEndpointPath, strings.NewReader("audio"))
	req.Header.Set(apiprotocol.AgentTokenHeader, secret)
	out := httptest.NewRecorder()
	server.Router().ServeHTTP(out, req)
	assert.Equal(http.StatusUnauthorized, out.Code)
	assert.Zero(st.calls)
}
