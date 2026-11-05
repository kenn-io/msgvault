package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	hexsource "go.kenn.io/msgvault/internal/hex"
)

type hexRecordingBoundaryStore struct {
	*mockStore

	calls int
	cap   int64
}

func (s *hexRecordingBoundaryStore) ImportHexRecording(_ context.Context, _ hexsource.RecordingUpload, body io.Reader, capBytes int64) (bool, error) {
	s.calls++
	s.cap = capBytes
	_, err := io.Copy(io.Discard, body)
	return true, err
}

func TestHexRecordingOwnerBoundaryAndSizeCap(t *testing.T) {
	recordings := map[string][]byte{"audio/wav": syntheticMuesliRecording(), "audio/mp4": syntheticMuesliM4A()}
	for _, tt := range []struct {
		name, key, size, media string
		cap                    int64
		status                 int
		hash                   string
	}{
		{"owner", meetingImportTestAPIKey, "", "audio/wav", 0, 200, ""},
		{"m4a", meetingImportTestAPIKey, "", "audio/mp4", 0, 415, ""},
		{"unauthenticated", "", "", "audio/wav", 0, 401, ""},
		{"over cap", meetingImportTestAPIKey, strconv.Itoa(len(recordings["audio/wav"]) + 1), "audio/wav", int64(len(recordings["audio/wav"])), 413, ""},
		{"default cap", meetingImportTestAPIKey, "4294967297", "audio/wav", 0, 413, ""},
		{"invalid size", meetingImportTestAPIKey, "-1", "audio/wav", 0, 422, ""},
		{"invalid hash", meetingImportTestAPIKey, "", "audio/wav", 0, 422, strings.Repeat("0", 64)},
		{"unsupported media", meetingImportTestAPIKey, "12", "application/octet-stream", 0, 415, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			recording := recordings[tt.media]
			if recording == nil {
				recording = recordings["audio/wav"]
			}
			digest := sha256.Sum256(recording)
			size := strconv.Itoa(len(recording))
			if tt.size != "" {
				size = tt.size
			}
			hash := hex.EncodeToString(digest[:])
			if tt.hash != "" {
				hash = tt.hash
			}

			st := &hexRecordingBoundaryStore{mockStore: &mockStore{stats: &StoreStats{}}}
			cfg := &config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey, HexRecordingMaxBytes: tt.cap}}
			server := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
			url := fmt.Sprintf("%s?track=system&identifier=recorder&account_email=user%%40example.com&message_id=42&sha256=%s&size=%s", hexRecordingEndpointPath, hash, size)
			req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(recording))
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

func TestHexRecordingReceivesBodyBeforeTakingOperationGate(t *testing.T) {
	recording := syntheticMuesliRecording()
	digest := sha256.Sum256(recording)
	url := fmt.Sprintf("%s?track=system&identifier=recorder&account_email=user%%40example.com&message_id=42&sha256=%s&size=%d", hexRecordingEndpointPath, hex.EncodeToString(digest[:]), len(recording))
	body := &pausedRecordingReader{reader: bytes.NewReader(recording), started: make(chan struct{}), resume: make(chan struct{})}
	gate := NewSerialOperationGate()
	st := &hexRecordingBoundaryStore{mockStore: &mockStore{stats: &StoreStats{}}}
	server := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey}},
		Store:         st,
		Logger:        testLogger(),
		OperationGate: gate,
	})
	req := httptest.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", "audio/wav")
	req.Header.Set("X-Api-Key", meetingImportTestAPIKey)
	resp := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.Router().ServeHTTP(resp, req)
		close(done)
	}()
	<-body.started
	_, _, held := gate.Holder()
	var release func()
	var acquired bool
	if !held {
		release, acquired = gate.BeginWork()
	}
	if release != nil {
		release()
	}
	close(body.resume)
	<-done
	assert := assert.New(t)
	assert.False(held, "receiving a recording must not hold the archive operation gate")
	assert.True(acquired, "another archive operation can run while the upload body is being received")
	assert.Equal(http.StatusOK, resp.Code, resp.Body.String())
	assert.Equal(1, st.calls)
}

func TestHexRecordingRejectsDelegatedToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := &hexRecordingBoundaryStore{mockStore: &mockStore{stats: &StoreStats{}}}
	server := NewServer(&config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	registry := agentgrant.NewRegistry()
	server.agentGrants = registry
	_, secret, _, err := registry.Issue("synthetic agent", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "hex", Identifier: "recorder"}}, time.Time{})
	require.NoError(err)
	req := httptest.NewRequest(http.MethodPost, hexRecordingEndpointPath, strings.NewReader("audio"))
	req.Header.Set(apiprotocol.AgentTokenHeader, secret)
	out := httptest.NewRecorder()
	server.Router().ServeHTTP(out, req)
	assert.Equal(http.StatusUnauthorized, out.Code)
	assert.Zero(st.calls)
}
