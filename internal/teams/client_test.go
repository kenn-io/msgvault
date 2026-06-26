package teams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientGetJSONPaging(t *testing.T) {
	var calls atomic.Int32
	serverURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"value":[{"id":"a"}],"@odata.nextLink":"` + serverURL + `/page2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"value":[{"id":"b"}],"@odata.deltaLink":"DELTA"}`))
	}))
	serverURL = srv.URL
	defer srv.Close()

	c := NewClient(srv.URL, func(context.Context) (string, error) { return "test-token", nil }, 50)
	var got []Chat
	delta, err := pageThrough[Chat](context.Background(), c, "/me/chats", func(page []Chat) { got = append(got, page...) })
	require.NoError(t, err)
	assert.Equal(t, "DELTA", delta)
	assert.Len(t, got, 2)
}

func TestClientRejectsOffOriginAbsoluteURLBeforeAuth(t *testing.T) {
	assert := assert.New(t)
	var attackerAuth atomic.Value
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"value":[]}`))
	}))
	defer attacker.Close()

	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"value":[]}`))
	}))
	defer graph.Close()

	var tokenCalls atomic.Int32
	c := NewClient(graph.URL, func(context.Context) (string, error) {
		tokenCalls.Add(1)
		return "secret-token", nil
	}, 50)

	_, err := c.GetRaw(context.Background(), attacker.URL+"/hostedContents/1/$value")
	require.Error(t, err)
	assert.Contains(err.Error(), "off-origin")
	assert.EqualValues(0, tokenCalls.Load(), "off-origin URLs must be rejected before requesting a token")
	assert.Nil(attackerAuth.Load(), "attacker server must not receive Authorization")
}

func TestClientRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"value":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	_, err := pageThrough[Chat](context.Background(), c, "/x", func([]Chat) {})
	require.NoError(t, err)
	assert.EqualValues(t, 2, calls.Load())
}

func TestClientContextCancelDuringRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30") // long wait so cancellation wins
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := pageThrough[Chat](ctx, c, "/x", func([]Chat) {})
	require.Error(t, err)
}

func TestListChatsAndMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/me/chats/") && strings.Contains(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"value":[{"id":"m1","createdDateTime":"2025-01-01T00:00:00Z","body":{"contentType":"text","content":"hi"}}]}`))
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"19:x@thread.v2","chatType":"oneOnOne"}]}`))
		default:
			http.Error(w, "no", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	require := require.New(t)
	assert := assert.New(t)
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	chats, err := c.ListChats(context.Background())
	require.NoError(err)
	require.Len(chats, 1)

	msgs, _, err := c.ListChatMessages(context.Background(), chats[0].ID, "", "")
	require.NoError(err)
	require.Len(msgs, 1)
	assert.Equal("m1", msgs[0].ID)
}

func TestListChatMessagesUsesInclusiveCursor(t *testing.T) {
	assert := assert.New(t)
	var filter string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filter = r.URL.Query().Get("$filter")
		_, _ = w.Write([]byte(`{"value":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 50)
	_, _, err := c.ListChatMessages(context.Background(), "19:x@thread.v2", "2025-01-01T00:00:00Z", "")
	require.NoError(t, err)

	assert.Equal("lastModifiedDateTime ge 2025-01-01T00:00:00Z", filter)
}
