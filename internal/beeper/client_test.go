package beeper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		http.Error(w, `{"error":"nope"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, testToken, 1000)
	_, err := c.GetMessage(context.Background(), "!c:x", "1")
	require.Error(err)
	assert.ErrorIs(err, ErrNotFound)
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
