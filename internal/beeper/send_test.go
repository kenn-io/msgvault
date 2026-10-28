package beeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendMessageAndUploadAsset(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		assert.Equal("Bearer test-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/assets/upload":
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			if !assert.NoError(r.ParseMultipartForm(1024)) { // #nosec G120 -- MaxBytesReader bounds the body above; gosec does not track this field assignment.
				return
			}
			defer func() { assert.NoError(r.MultipartForm.RemoveAll()) }()
			f, header, err := r.FormFile("file")
			if !assert.NoError(err) {
				return
			}
			defer func() { assert.NoError(f.Close()) }()
			data, err := io.ReadAll(f)
			if !assert.NoError(err) {
				return
			}
			assert.Equal("content", string(data))
			assert.Equal("example.txt", header.Filename)
			assert.Equal("text/plain; charset=utf-8", header.Header.Get("Content-Type"))
			assert.Empty(r.MultipartForm.Value)
			_, _ = io.WriteString(w, `{"uploadID":"upload-1"}`)
		case "/v1/chats/!chat:beeper.local/messages":
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				return
			}
			assert.Equal(map[string]any{"text": "hello", "replyToMessageID": "original", "attachment": map[string]any{"uploadID": "upload-1"}}, body)
			_, _ = io.WriteString(w, `{"chatID":"!chat:beeper.local","pendingMessageID":"pending-1"}`)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, func(_ context.Context) (string, error) { return "test-token", nil }, 1000)
	upload, err := client.UploadAsset(t.Context(), strings.NewReader("content"), "example.txt")
	require.NoError(err)
	pending, err := client.SendMessage(t.Context(), "!chat:beeper.local", SendParams{Text: "hello", ReplyToMessageID: "original", UploadID: upload})
	require.NoError(err)
	assert.Equal(SendResult{ChatID: "!chat:beeper.local", PendingMessageID: "pending-1"}, pending)
	assert.Equal([]string{"POST /v1/assets/upload", "POST /v1/chats/!chat:beeper.local/messages"}, requests)
}

func TestBeeperWritesNeverRetry(t *testing.T) {
	for _, status := range []int{307, 429, 500} {
		for _, operation := range []string{"send", "upload"} {
			t.Run(fmt.Sprintf("%s/%d", operation, status), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(status)
				}))
				defer server.Close()
				client := NewClient(server.URL, testToken, 1000)
				var err error
				switch operation {
				case "send":
					_, err = client.SendMessage(t.Context(), "chat", SendParams{Text: "hello"})
				case "upload":
					_, err = client.UploadAsset(t.Context(), strings.NewReader("content"), "example.txt")
				}
				var failure *WriteError
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, status, failure.Status)
				assert.Equal(t, 1, calls)
			})
		}
	}
}

type failedSendTransport struct{}

func (failedSendTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection lost")
}

func TestSendMessageUnknownOutcome(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := NewClient("http://localhost", testToken, 1000)
	client.http = &http.Client{Transport: failedSendTransport{}}
	_, err := client.SendMessage(t.Context(), "chat", SendParams{Text: "hello"})
	var failure *WriteError
	require.ErrorAs(err, &failure)
	assert.Zero(failure.Status)
	for _, body := range []string{`not json`, `{}`, `{"pendingMessageID":"pending-1"}`, `{"chatID":"chat"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		client := NewClient(server.URL, testToken, 1000)
		_, err = client.SendMessage(t.Context(), "chat", SendParams{Text: "hello"})
		require.ErrorAs(err, &failure)
		assert.Zero(failure.Status)
		server.Close()
	}
}

func TestUploadAssetEarlyResponse(t *testing.T) {
	for _, tc := range []struct {
		name, response, uploadID string
		status                   int
	}{
		{name: "rejection", response: `{"error":"unsupported upload"}`, status: http.StatusBadRequest},
		{name: "receipt", response: `{"uploadID":"upload-1"}`, uploadID: "upload-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/v1/assets/upload", r.URL.Path)
				// Answer before reading the body; the large upload cannot finish
				// before this response interrupts the multipart writer.
				w.Header().Set("Connection", "close")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			client := NewClient(server.URL, testToken, 1000)
			uploadID, err := client.UploadAsset(t.Context(), strings.NewReader(strings.Repeat("x", 32<<20)), "example.txt")
			assert.Equal(t, int32(1), calls.Load())
			if tc.status != 0 {
				var failure *WriteError
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, tc.status, failure.Status)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.uploadID, uploadID)
		})
	}
}

func TestUploadUnknownExtensionSniffsContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if !assert.NoError(t, r.ParseMultipartForm(1024)) { // #nosec G120 -- MaxBytesReader bounds the body above; gosec does not track this field assignment.
			return
		}
		defer func() { assert.NoError(t, r.MultipartForm.RemoveAll()) }()
		f, header, err := r.FormFile("file")
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "image/png", header.Header.Get("Content-Type"))
		defer func() { assert.NoError(t, f.Close()) }()
		data, err := io.ReadAll(f)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "\x89PNG\r\n\x1a\nexample", string(data))
		_, _ = io.WriteString(w, `{"uploadID":"upload-1"}`)
	}))
	defer server.Close()
	client := NewClient(server.URL, testToken, 1000)
	_, err := client.UploadAsset(t.Context(), strings.NewReader("\x89PNG\r\n\x1a\nexample"), "file")
	require.NoError(t, err)
}

func TestVerifyArchivedMessage(t *testing.T) {
	sentAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, id, chat string
		delta          time.Duration
		ok             bool
	}{
		{"match", "message", "chat", 0, true}, {"precision", "message", "chat", archivedSampleTolerance, true},
		{"negative precision", "message", "chat", -archivedSampleTolerance, true},
		{"timestamp", "message", "chat", time.Hour, false}, {"id", "other", "chat", 0, false}, {"chat", "message", "other", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(Message{ID: tc.id, ChatID: tc.chat, Timestamp: sentAt.Add(tc.delta)})
			}))
			defer server.Close()
			err := NewClient(server.URL, testToken, 1000).VerifyArchivedMessage(t.Context(), "chat", "message", sentAt)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrArchivedMessageMismatch)
			}
		})
	}
}

func TestSendMessageTokenFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context) (string, error) { return "", errors.New("token unavailable") }, 1000)
	_, err := client.SendMessage(t.Context(), "chat", SendParams{Text: "hello"})
	require.Error(t, err)
	var writeErr *WriteError
	assert.NotErrorAs(t, err, &writeErr)
	assert.Zero(t, calls)
}
