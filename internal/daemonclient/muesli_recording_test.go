package daemonclient

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
)

func TestMuesliRecordingStreamsAuthenticatedBytesAndRetries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	data := []byte("RIFF synthetic recording bytes")
	file, err := os.CreateTemp(t.TempDir(), "recording")
	require.NoError(err)
	t.Cleanup(func() { _ = file.Close() })
	_, err = file.Write(data)
	require.NoError(err)
	hash := sha256.Sum256(data)
	recording := &muesli.Recording{File: file, Hash: hex.EncodeToString(hash[:]), Size: int64(len(data)), MIMEType: "audio/wav"}
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal("owner-key", r.Header.Get("X-Api-Key"))
		assert.Equal("audio/wav", r.Header.Get("Content-Type"))
		assert.Equal(recording.Size, r.ContentLength)
		assert.Equal("/api/v1/import/muesli/recording", r.URL.Path)
		assert.Equal("42", r.URL.Query().Get("message_id"))
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(data, body)
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"operation_in_progress","message":"busy"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"updated","message_id":42,"changed":true}`))
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, APIKey: "owner-key", HTTPClient: server.Client()})
	require.NoError(err)
	upload := muesli.RecordingUpload{Source: meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}, MessageID: 42, Hash: recording.Hash, Size: recording.Size, MIMEType: recording.MIMEType}
	require.NoError(client.ImportMuesliRecording(t.Context(), upload, recording))
	assert.Equal(2, calls)
}
