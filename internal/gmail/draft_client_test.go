package gmail

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDraftTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{
		httpClient:  &http.Client{Transport: &rewriteTransport{base: srv.URL, wrapped: http.DefaultTransport}},
		userID:      "me",
		concurrency: 1,
		logger:      testLoggerForDrafts(),
		rateLimiter: NewRateLimiter(1000),
	}
}

func testLoggerForDrafts() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestDraftClientWireMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("From: alice@example.com\r\n\r\nbody\r\n")
	paddedRaw := base64.URLEncoding.EncodeToString(raw)
	var paths []string
	serverHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		body, err := io.ReadAll(r.Body)
		require.NoError(err)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/gmail/v1/users/me/drafts":
			var request draftRequestJSON
			require.NoError(json.Unmarshal(body, &request))
			assert.Equal("thread-parent", request.Message.ThreadID)
			decoded, decodeErr := decodeBase64URL(request.Message.Raw)
			require.NoError(decodeErr)
			assert.Equal(raw, decoded)
			_, _ = w.Write([]byte(`{"id":"draft-1","message":{"id":"message-1","threadId":"thread-parent","labelIds":["DRAFT"],"raw":"` + paddedRaw + `"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/drafts/draft-1"):
			_, _ = w.Write([]byte(`{"id":"draft-1","message":{"id":"message-1","threadId":"thread-parent","raw":"` + paddedRaw + `"}}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/drafts/draft-1"):
			var request draftRequestJSON
			require.NoError(json.Unmarshal(body, &request))
			assert.Equal("draft-1", request.ID)
			_, _ = w.Write([]byte(`{"id":"draft-1","message":{"id":"message-2","threadId":"thread-parent","raw":"` + paddedRaw + `"}}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/drafts/draft-1"):
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/settings/sendAs":
			_, _ = w.Write([]byte(`{"sendAs":[{"sendAsEmail":"alice@example.com","displayName":"Alice","isPrimary":true,"isDefault":true,"verificationStatus":"accepted"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	client := newDraftTestClient(t, serverHandler)

	created, err := client.CreateDraft(t.Context(), raw, "thread-parent")
	require.NoError(err)
	assert.Equal("draft-1", created.ID)
	assert.Equal("message-1", created.Message.ID)
	assert.Equal(raw, created.Message.Raw)

	got, err := client.GetDraft(t.Context(), "draft-1")
	require.NoError(err)
	assert.Equal(raw, got.Message.Raw)

	updated, err := client.UpdateDraft(t.Context(), "draft-1", raw, "thread-parent")
	require.NoError(err)
	assert.Equal("message-2", updated.Message.ID)
	require.NoError(client.DeleteDraft(t.Context(), "draft-1"))

	sendAs, err := client.ListSendAs(t.Context())
	require.NoError(err)
	require.Len(sendAs, 1)
	assert.Equal("alice@example.com", sendAs[0].Email)
	assert.True(sendAs[0].Primary)
	assert.Equal([]string{
		"/gmail/v1/users/me/drafts",
		"/gmail/v1/users/me/drafts/draft-1",
		"/gmail/v1/users/me/drafts/draft-1",
		"/gmail/v1/users/me/drafts/draft-1",
		"/gmail/v1/users/me/settings/sendAs",
	}, paths)
}

func TestDraftMutation429IsRemoteUnknownWithoutReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429}}`))
	}))

	_, err := client.CreateDraft(t.Context(), []byte("body"), "thread")
	require.Error(err)
	var writeErr *DraftWriteError
	require.ErrorAs(err, &writeErr)
	assert.Equal("remote_unknown", writeErr.Code)
	assert.Equal(int32(1), requests.Load())
}

func TestDraftMalformedSuccessIsRemoteUnknown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"draft-1","message":{}}`))
	}))

	_, err := client.CreateDraft(t.Context(), []byte("body"), "thread")
	require.Error(err)
	var writeErr *DraftWriteError
	require.ErrorAs(err, &writeErr)
	assert.Equal("remote_unknown", writeErr.Code)
}
