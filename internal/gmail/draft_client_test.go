package gmail

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
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
			w.WriteHeader(http.StatusNoContent)
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

func TestDraftDeleteAcceptsEmptySuccessBody(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodDelete, r.Method)
				w.WriteHeader(status)
			}))
			require.NoError(t, client.DeleteDraft(t.Context(), "draft-1"))
		})
	}
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

type draftRoundTripFunc func(*http.Request) (*http.Response, error)

func (f draftRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type draftReadErrorBody struct{}

func (draftReadErrorBody) Read([]byte) (int, error) { return 0, errors.New("response read failed") }
func (draftReadErrorBody) Close() error             { return nil }

type draftUnauthorizedTokenSource struct{}

func (draftUnauthorizedTokenSource) Token() (*oauth2.Token, error) {
	return nil, &oauth2.RetrieveError{
		Response: &http.Response{StatusCode: http.StatusUnauthorized},
		Body:     []byte(`{"error":"unauthorized_client"}`),
	}
}

func newDraftMutationClient(transport http.RoundTripper) *Client {
	return &Client{
		httpClient:  &http.Client{Transport: transport},
		userID:      "me",
		concurrency: 1,
		logger:      testLoggerForDrafts(),
		rateLimiter: NewRateLimiter(1000),
	}
}

func TestDraftMutationUncertainFailuresMakeOneRequest(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "server error", status: http.StatusInternalServerError},
		{name: "service unavailable", status: http.StatusServiceUnavailable},
		{name: "rate limit", status: http.StatusForbidden, body: `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			requests := 0
			client := newDraftMutationClient(draftRoundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				return &http.Response{
					StatusCode: tt.status, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(tt.body)),
				}, nil
			}))
			_, err := client.CreateDraft(t.Context(), []byte("body"), "thread")
			requirements.Error(err)
			var writeErr *DraftWriteError
			requirements.ErrorAs(err, &writeErr)
			assertions.Equal("remote_unknown", writeErr.Code)
			assertions.Equal(1, requests)
		})
	}
}

func TestDraftMutationTransportAndReadFailuresAreRemoteUnknown(t *testing.T) {
	tests := []struct {
		name string
		base http.RoundTripper
	}{
		{
			name: "transport",
			base: draftRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection reset")
			}),
		},
		{
			name: "response read",
			base: draftRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK, Header: make(http.Header), Body: draftReadErrorBody{},
				}, nil
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			_, err := newDraftMutationClient(tt.base).CreateDraft(t.Context(), []byte("body"), "thread")
			requirements.Error(err)
			var writeErr *DraftWriteError
			requirements.ErrorAs(err, &writeErr)
			assertions.Equal("remote_unknown", writeErr.Code)
		})
	}
}

func TestDraftMutationTokenFailureMakesNoGmailRequest(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	client := NewClient(draftUnauthorizedTokenSource{}, WithRateLimiter(NewRateLimiter(1000)))
	transport, ok := client.httpClient.Transport.(*oauth2.Transport)
	requirements.True(ok)
	requests := 0
	transport.Base = draftRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("Gmail request must not be sent")
	})

	_, err := client.CreateDraft(t.Context(), []byte("body"), "thread")
	requirements.Error(err)
	var writeErr *DraftWriteError
	requirements.ErrorAs(err, &writeErr)
	assertions.Equal("auth_failed", writeErr.Code)
	assertions.Zero(requests)
}

func TestIsInsufficientScopeErrorRecognizesProviderMessages(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    bool
	}{
		{
			name:    "access token scope insufficient",
			message: "googleapi: Error 403: ACCESS_TOKEN_SCOPE_INSUFFICIENT",
			want:    true,
		},
		{
			name:    "insufficient authentication scopes",
			message: "googleapi: Error 403: insufficient authentication scopes",
			want:    true,
		},
		{
			name:    "insufficient permission",
			message: "googleapi: Error 403: Insufficient Permission",
			want:    true,
		},
		{
			name:    "generic insufficient text",
			message: "provider returned insufficient data",
			want:    false,
		},
		{
			name:    "scope shorthand",
			message: "provider returned insufficient scope",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsInsufficientScopeError(tt.message))
		})
	}
}

func TestExistingTrashRetriesServerError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/gmail/v1/users/me/messages/message-1/trash", r.URL.Path)
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	require.NoError(client.TrashMessage(t.Context(), "message-1"))
	assert.Equal(int32(2), requests.Load())
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
