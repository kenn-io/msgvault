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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendMessageAndUploadAsset(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/assets/upload":
			require.NoError(t, r.ParseMultipartForm(1024))
			defer r.MultipartForm.RemoveAll()
			f, header, err := r.FormFile("file")
			require.NoError(t, err)
			defer f.Close()
			data, err := io.ReadAll(f)
			require.NoError(t, err)
			assert.Equal(t, "content", string(data))
			assert.Equal(t, "example.txt", header.Filename)
			assert.Equal(t, "text/plain; charset=utf-8", header.Header.Get("Content-Type"))
			assert.Empty(t, r.MultipartForm.Value)
			_, _ = io.WriteString(w, `{"uploadID":"upload-1"}`)
		case "/v1/chats/!chat:beeper.local/messages":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, map[string]any{"text": "hello", "replyToMessageID": "original", "attachment": map[string]any{"uploadID": "upload-1"}}, body)
			_, _ = io.WriteString(w, `{"chatID":"!chat:beeper.local","pendingMessageID":"pending-1"}`)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, func(_ context.Context) (string, error) { return "test-token", nil }, 1000)
	upload, err := client.UploadAsset(t.Context(), strings.NewReader("content"), "example.txt")
	require.NoError(t, err)
	pending, err := client.SendMessage(t.Context(), "!chat:beeper.local", SendParams{Text: "hello", ReplyToMessageID: "original", UploadID: upload})
	require.NoError(t, err)
	assert.Equal(t, SendResult{ChatID: "!chat:beeper.local", PendingMessageID: "pending-1"}, pending)
	assert.Equal(t, []string{"POST /v1/assets/upload", "POST /v1/chats/!chat:beeper.local/messages"}, requests)
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
	client := NewClient("http://localhost", testToken, 1000)
	client.http = &http.Client{Transport: failedSendTransport{}}
	_, err := client.SendMessage(t.Context(), "chat", SendParams{Text: "hello"})
	var failure *WriteError
	require.ErrorAs(t, err, &failure)
	assert.Zero(t, failure.Status)
	for _, body := range []string{`not json`, `{}`, `{"pendingMessageID":"pending-1"}`, `{"chatID":"chat"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		client := NewClient(server.URL, testToken, 1000)
		_, err = client.SendMessage(t.Context(), "chat", SendParams{Text: "hello"})
		require.ErrorAs(t, err, &failure)
		assert.Zero(t, failure.Status)
		server.Close()
	}
}

func TestUploadUnknownExtensionSniffsContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseMultipartForm(1024))
		defer r.MultipartForm.RemoveAll()
		f, header, err := r.FormFile("file")
		require.NoError(t, err)
		assert.Equal(t, "image/png", header.Header.Get("Content-Type"))
		defer f.Close()
		data, err := io.ReadAll(f)
		require.NoError(t, err)
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
	assert.False(t, errors.As(err, &writeErr))
	assert.Zero(t, calls)
}
