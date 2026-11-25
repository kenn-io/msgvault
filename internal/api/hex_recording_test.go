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
		delegated              bool
	}{
		{"owner", meetingImportTestAPIKey, "", "audio/wav", 0, 200, "", false},
		{"delegated", "", "", "audio/wav", 0, 401, "", true},
		{"m4a", meetingImportTestAPIKey, "", "audio/mp4", 0, 415, "", false},
		{"unauthenticated", "", "", "audio/wav", 0, 401, "", false},
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
			cfg := &config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey, HexRecordingMaxBytes: tt.cap, AgentAccess: tt.delegated}}
			server := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger(), OperationGate: NewSerialOperationGate()})

			var secret string
			if tt.delegated {
				registry := agentgrant.NewRegistry()
				server.agentGrants = registry
				_, token, _, err := registry.Issue("synthetic agent", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "hex", Identifier: "recorder"}}, time.Time{})
				require.NoError(t, err)
				secret = token
			}
			url := fmt.Sprintf("%s?track=system&identifier=recorder&account_email=user%%40example.com&message_id=42&sha256=%s&size=%s", hexRecordingEndpointPath, hash, size)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(recording))
			req.Header.Set("Content-Type", tt.media)
			req.Header.Set("X-Api-Key", tt.key)
			if tt.delegated {
				req.Header.Set(apiprotocol.AgentTokenHeader, secret)
			}
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
