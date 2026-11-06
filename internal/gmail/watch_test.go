package gmail

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatchMailboxWide(t *testing.T) {
	assertions := assert.New(t)
	expiration := time.Now().Add(90 * time.Second).Truncate(time.Millisecond)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/gmail/v1/users/me/watch", r.URL.Path)
		var body map[string]any
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		assert.Equal(t, map[string]any{"topicName": "projects/example/topics/mail"}, body)
		_, err := fmt.Fprintf(w, `{"historyId":"12345","expiration":%q}`, strconv.FormatInt(expiration.UnixMilli(), 10))
		assert.NoError(t, err)
	}))
	defer server.Close()
	c := NewClient(nil)
	c.httpClient = &http.Client{Transport: &rewriteTransport{base: server.URL, wrapped: http.DefaultTransport}}
	for range 2 { // Renewal uses the same mailbox-wide request and never supplies a cursor.
		result, err := c.Watch(context.Background(), "projects/example/topics/mail")
		require.NoError(t, err)
		assertions.Equal("12345", result.HistoryID)
		assertions.Equal(expiration, result.Expiration)
	}
	assertions.Equal(int32(2), requests.Load())
	assertions.Equal(100, OpWatch.Cost())
}

func TestWatchRejectsInvalidResponse(t *testing.T) {
	future := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	for _, body := range []string{
		`{}`, `{"historyId":"0","expiration":"` + future + `"}`,
		`{"historyId":"-1","expiration":"` + future + `"}`,
		`{"historyId":"18446744073709551616","expiration":"` + future + `"}`,
		`{"historyId":"1","expiration":"0"}`, `{"historyId":"1","expiration":"-1"}`,
		`{"historyId":"1","expiration":"9223372036854775808"}`,
		`{"historyId":"1","expiration":"1"}`, `{"historyId":1,"expiration":"` + future + `"}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, err := w.Write([]byte(body)); assert.NoError(t, err) }))
			defer server.Close()
			c := NewClient(nil)
			c.httpClient = &http.Client{Transport: &rewriteTransport{base: server.URL, wrapped: http.DefaultTransport}}
			result, err := c.Watch(context.Background(), "projects/example/topics/mail")
			require.Error(t, err)
			assert.Nil(t, result)
		})
	}
}

func TestWatchRejectsTopicBeforeRequest(t *testing.T) {
	c := NewClient(nil)
	for _, topic := range []string{"", "mail", "projects/example/subscriptions/mail", "projects/example/topics/mail?x=1", "projects/example/topics/../mail"} {
		_, err := c.Watch(context.Background(), topic)
		require.Error(t, err)
	}
}

func TestWatchFailureAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	c := NewClient(nil)
	c.httpClient = &http.Client{Transport: &rewriteTransport{base: server.URL, wrapped: http.DefaultTransport}}
	_, err := c.Watch(context.Background(), "projects/example/topics/mail")
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Watch(ctx, "projects/example/topics/mail")
	require.ErrorIs(t, err, context.Canceled)
}
