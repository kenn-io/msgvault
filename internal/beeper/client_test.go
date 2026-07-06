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
	require.NoError(t, err)
	assert.Empty(t, accounts)
	assert.EqualValues(t, 3, calls.Load())
}

func TestClientUnauthorizedMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, testToken, 1000)
	_, err := c.ListAccounts(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add-beeper")
	assert.Contains(t, err.Error(), "Beeper Desktop")
}

func TestClientNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, testToken, 1000)
	_, err := c.GetMessage(context.Background(), "!c:x", "1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestClientContextCancelDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	c := NewClient(srv.URL, testToken, 1000)
	start := time.Now()
	_, err := c.ListAccounts(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second, "must abort backoff on ctx cancel")
}

func TestClientSendsBearerToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, testToken, 1000)
	_, err := c.ListAccounts(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-token", got)
}

func TestSearchChatsParams(t *testing.T) {
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
	require.NoError(t, err)
	require.Len(t, out.Items, 1)
	assert.Equal(t, "!a:x", out.Items[0].ID, "account and activity filters must both apply")
}

func TestListMessagesPagination(t *testing.T) {
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

	// Newest page first, then walk older to exhaustion.
	var total int
	page, err := c.ListMessagesPage(ctx, "!p:x", "", "")
	require.NoError(t, err)
	total += len(page.Items)
	for page.HasMore {
		page, err = c.ListMessagesPage(ctx, "!p:x", page.OldestCursor, "before")
		require.NoError(t, err)
		total += len(page.Items)
	}
	assert.Equal(t, 45, total)

	// Walk newer from a mid-chat cursor: 24 newer messages remain, so the
	// first page is full and more are available.
	after, err := c.ListMessagesPage(ctx, "!p:x", "20", "after")
	require.NoError(t, err)
	assert.Len(t, after.Items, 20)
	assert.True(t, after.HasMore)
}
