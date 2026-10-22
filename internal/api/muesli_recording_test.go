package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

func syntheticMuesliRecording() []byte {
	return []byte("RIFF\x04\x00\x00\x00WAVE")
}

func syntheticMuesliM4A() []byte {
	data := make([]byte, 32)
	binary.BigEndian.PutUint32(data, 16)
	copy(data[4:], "ftyp")
	copy(data[8:], "M4A ")
	binary.BigEndian.PutUint32(data[16:], 8)
	copy(data[20:], "moov")
	binary.BigEndian.PutUint32(data[24:], 8)
	copy(data[28:], "mdat")
	return data
}

func TestMuesliRecordingOwnerBoundaryAndSizeCap(t *testing.T) {
	recordings := map[string][]byte{"audio/wav": syntheticMuesliRecording(), "audio/mp4": syntheticMuesliM4A()}
	for _, tt := range []struct {
		name, key, size, media string
		cap                    int64
		status                 int
		hash                   string
	}{
		{"owner", meetingImportTestAPIKey, "", "audio/wav", 0, 200, ""},
		{"m4a", meetingImportTestAPIKey, "", "audio/mp4", 0, 200, ""},
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

			st := &recordingBoundaryStore{mockStore: &mockStore{stats: &StoreStats{}}}
			cfg := &config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey, MuesliRecordingMaxBytes: tt.cap}}
			server := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
			url := fmt.Sprintf("%s?identifier=recorder&account_email=user%%40example.com&message_id=42&sha256=%s&size=%s", muesliRecordingEndpointPath, hash, size)
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

type pausedRecordingReader struct {
	reader  *bytes.Reader
	started chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (r *pausedRecordingReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.started)
		<-r.resume
	})
	n, err := r.reader.Read(p)
	if err == io.EOF {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("read paused recording body: %w", err)
	}
	return n, nil
}

func TestMuesliRecordingReceivesBodyBeforeTakingOperationGate(t *testing.T) {
	recording := syntheticMuesliRecording()
	digest := sha256.Sum256(recording)
	url := fmt.Sprintf("%s?identifier=recorder&account_email=user%%40example.com&message_id=42&sha256=%s&size=%d", muesliRecordingEndpointPath, hex.EncodeToString(digest[:]), len(recording))
	body := &pausedRecordingReader{reader: bytes.NewReader(recording), started: make(chan struct{}), resume: make(chan struct{})}
	gate := NewSerialOperationGate()
	st := &recordingBoundaryStore{mockStore: &mockStore{stats: &StoreStats{}}}
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
