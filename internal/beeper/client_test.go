package beeper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientRetriesOn429(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, testToken, 1000)
	accounts, err := c.ListAccounts(context.Background())
	require.NoError(err)
	assert.Empty(accounts)
	assert.EqualValues(3, calls.Load())
}

func TestClientCapsOversizedRetryAfter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "86400")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, testToken, 1000)
	c.retryAfterMax = 10 * time.Millisecond
	start := time.Now()
	accounts, err := c.ListAccounts(context.Background())
	require.NoError(err)
	assert.Empty(accounts)
	assert.EqualValues(2, calls.Load())
	assert.Less(time.Since(start), time.Second,
		"provider Retry-After must not park a sync for the requested day")
}

func TestClientUnauthorizedMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, testToken, 1000)
	_, err := c.ListAccounts(context.Background())
	require.Error(err)
	assert.Contains(err.Error(), "add-beeper")
	assert.Contains(err.Error(), "Beeper Desktop")
}

func TestClientNotFound(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, expiredAssetBody, http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, testToken, 1000)
	_, err := c.GetMessage(context.Background(), "!c:x", "1")
	require.Error(err)
	require.ErrorIs(err, ErrNotFound)
	assert.NotErrorIs(err, ErrAssetUnavailable)
}

func TestClientContextCancelDuringBackoff(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	c := NewClient(srv.URL, testToken, 1000)
	start := time.Now()
	_, err := c.ListAccounts(ctx)
	require.Error(err)
	require.ErrorIs(err, context.DeadlineExceeded)
	assert.Less(time.Since(start), 5*time.Second, "must abort backoff on ctx cancel")
}

func TestClientSendsBearerToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, testToken, 1000)
	_, err := c.ListAccounts(context.Background())
	require.NoError(err)
	assert.Equal("Bearer test-token", got)
}

func TestSearchChatsParams(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	f.addChat(&fakeChat{ID: "!a:x", AccountID: "signal", Network: "Signal", Title: "A", Type: "single",
		LastActivity: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)})
	f.addChat(&fakeChat{ID: "!b:x", AccountID: "signal", Network: "Signal", Title: "B", Type: "single",
		LastActivity: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	f.addChat(&fakeChat{ID: "!c:x", AccountID: "whatsapp", Network: "WhatsApp", Title: "C", Type: "single",
		LastActivity: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)})
	srv := f.server()
	defer srv.Close()

	c := NewClient(srv.URL, testToken, 1000)
	out, err := c.SearchChats(context.Background(), SearchChatsParams{
		AccountID:         "signal",
		LastActivityAfter: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	require.Len(out.Items, 1)
	assert.Equal("!a:x", out.Items[0].ID, "account and activity filters must both apply")
}

func TestListMessagesPagination(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newFakeBeeper(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ch := &fakeChat{ID: "!p:x", AccountID: "signal", Network: "Signal", Title: "P", Type: "single", LastActivity: base}
	for i := range 45 {
		ch.Msgs = append(ch.Msgs, fakeMsg{
			ID: "m" + strconv.Itoa(i), SortKey: i, Timestamp: base.Add(time.Duration(i) * time.Minute),
			Text: "msg", SenderID: "@a:x", SenderName: "A",
		})
	}
	f.addChat(ch)
	srv := f.server()
	defer srv.Close()

	c := NewClient(srv.URL, testToken, 1000)
	ctx := context.Background()

	// Newest page first, then walk older until the empty end-of-history page
	// (the live API's hasMore flag is not a reliable termination signal).
	var total int
	cursor, direction := "", ""
	for range 10 {
		page, err := c.ListMessagesPage(ctx, "!p:x", cursor, direction)
		require.NoError(err)
		if len(page.Items) == 0 {
			assert.Empty(page.OldestCursor, "empty page carries null cursors")
			break
		}
		total += len(page.Items)
		cursor, direction = page.OldestCursor, "before"
	}
	assert.Equal(45, total)

	// Walk newer from a mid-chat cursor: the page advances exclusively.
	after, err := c.ListMessagesPage(ctx, "!p:x", "20", "after")
	require.NoError(err)
	assert.Len(after.Items, 20)
}

// expiredAssetBody is the error body Beeper returns when the network has
// deleted the media from its servers.
const expiredAssetBody = `{"message":"Failed to download asset: Transfer failed for localmxc://local-whatsapp/example: downloadFileWithParams failed: Media is no longer available on WhatsApp"}`

type assetResponse struct {
	status     int
	retryAfter string
	body       string
}

// assetServer serves responses in order, repeating the last one, and counts
// requests.
func assetServer(t *testing.T, responses ...assetResponse) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(calls.Add(1))
		resp := responses[min(n, len(responses))-1]
		if resp.retryAfter != "" {
			w.Header().Set("Retry-After", resp.retryAfter)
		}
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, testToken, 10000)
	c.retryAfterMax = time.Millisecond
	return c, &calls
}

func TestGetAssetBytesClassification(t *testing.T) {
	tests := []struct {
		name        string
		responses   []assetResponse
		wantCalls   int32
		wantErr     bool
		unavailable bool
	}{
		{
			name:        "permanent error is not retried",
			responses:   []assetResponse{{status: http.StatusInternalServerError, body: expiredAssetBody}},
			wantCalls:   1,
			wantErr:     true,
			unavailable: true,
		},
		{
			name:        "permanent phrase on a client error",
			responses:   []assetResponse{{status: http.StatusBadRequest, body: "MEDIA IS NO LONGER AVAILABLE"}},
			wantCalls:   1,
			wantErr:     true,
			unavailable: true,
		},
		{
			name:        "permanent phrase on not found",
			responses:   []assetResponse{{status: http.StatusNotFound, body: expiredAssetBody}},
			wantCalls:   1,
			wantErr:     true,
			unavailable: true,
		},
		{
			name: "throttling is transient whatever its body says",
			responses: []assetResponse{
				{status: http.StatusTooManyRequests, body: expiredAssetBody},
				{status: http.StatusTooManyRequests, body: expiredAssetBody},
				{status: http.StatusOK, body: "bytes"},
			},
			wantCalls: 3,
		},
		{
			name:      "throttling is transient but bounded",
			responses: []assetResponse{{status: http.StatusTooManyRequests, retryAfter: "0"}},
			wantCalls: 3,
			wantErr:   true,
		},
		{
			name:      "unknown server errors are capped",
			responses: []assetResponse{{status: http.StatusInternalServerError, body: `{"message":"boom"}`}},
			wantCalls: 3,
			wantErr:   true,
		},
		{
			name:      "server Retry-After counts toward the cap",
			responses: []assetResponse{{status: http.StatusServiceUnavailable, retryAfter: "0"}},
			wantCalls: 3,
			wantErr:   true,
		},
		{
			name: "throttling counts toward the asset retry cap",
			responses: []assetResponse{
				{status: http.StatusInternalServerError},
				{status: http.StatusTooManyRequests},
				{status: http.StatusTooManyRequests},
				{status: http.StatusInternalServerError},
				{status: http.StatusOK, body: "bytes"},
			},
			wantCalls: 3,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			c, calls := assetServer(t, tt.responses...)
			data, err := c.GetAssetBytes(context.Background(), "mxc://example.test/a", 1<<20)
			assert.Equal(tt.wantCalls, calls.Load())
			if !tt.wantErr {
				require.NoError(t, err)
				assert.Equal("bytes", string(data))
				return
			}
			require.Error(t, err)
			assert.Equal(tt.unavailable, errors.Is(err, ErrAssetUnavailable), "error: %v", err)
			assert.NotErrorIs(err, ErrAssetTooLarge)
		})
	}
}

func TestGetAssetBytesLargeErrorBodyIsNotSizeCap(t *testing.T) {
	assert := assert.New(t)
	body := expiredAssetBody + strings.Repeat(" ", 2048)
	c, calls := assetServer(t, assetResponse{status: http.StatusInternalServerError, body: body})
	_, err := c.GetAssetBytes(context.Background(), "mxc://example.test/a", 16)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAssetTooLarge, "an error body is not the asset")
	require.ErrorIs(t, err, ErrAssetUnavailable)
	assert.EqualValues(1, calls.Load())
}

func TestClientPermanentPhraseIgnoredOffAssets(t *testing.T) {
	assert := assert.New(t)
	c, calls := assetServer(t,
		assetResponse{status: http.StatusInternalServerError, body: expiredAssetBody},
		assetResponse{status: http.StatusOK, body: `[]`},
	)
	accounts, err := c.ListAccounts(context.Background())
	require.NoError(t, err)
	assert.Empty(accounts)
	assert.EqualValues(2, calls.Load())
}

func TestClientDoesNotSleepAfterFinalAttempt(t *testing.T) {
	assert := assert.New(t)
	c, calls := assetServer(t, assetResponse{status: http.StatusInternalServerError})
	c.retryAfterMax = 500 * time.Millisecond
	start := time.Now()
	_, err := c.GetAssetBytes(context.Background(), "mxc://example.test/a", 1<<20)
	require.Error(t, err)
	assert.EqualValues(3, calls.Load())
	// Two capped waits (1s total) separate three attempts; a third wait
	// after the last attempt would push this to at least 1.5s.
	assert.Less(time.Since(start), 1400*time.Millisecond)
}
