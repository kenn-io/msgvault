package tiktok

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixtureClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClient("fixture-access-token")
	require.NoError(t, err)
	c.baseURL = srv.URL
	c.http = srv.Client()
	now := time.Now()
	c.limiter = &Limiter{now: func() time.Time { return now }, wait: func(ctx context.Context, d time.Duration) error { now = now.Add(d); return ctx.Err() }}
	return c
}

func TestClientCommentsPreserveIDsAndPaging(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("fixture-access-token", r.Header.Get("Access-Token"))
		assertions.Empty(r.URL.Query().Get("access_token"))
		assertions.Equal("/business/comment/list/", r.URL.Path)
		assertions.Equal("account+example/", r.URL.Query().Get("business_id"))
		assertions.Equal("9007199254740993", r.URL.Query().Get("video_id"))
		assertions.Equal("ALL", r.URL.Query().Get("status"))
		assertions.Equal("30", r.URL.Query().Get("max_count"))
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"comments":[{"comment_id":9007199254740999,"video_id":"9007199254740993","text":"Synthetic comment","unique_identifier":"actor+example/","create_time":1700000000}],"cursor":9007199254740997,"has_more":true}}`)
	})
	page, err := c.Comments(t.Context(), "account+example/", "9007199254740993", "", "")
	requirements.NoError(err)
	requirements.Len(page.Comments, 1)
	assertions.Equal(ID("9007199254740999"), page.Comments[0].ID)
	assertions.Equal(ID("9007199254740997"), page.Cursor)
	assertions.True(page.HasMore)
}

func TestClientFailuresAreBoundedAndSanitized(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		status, calls int
	}{
		{"denied", `{"code":40105,"message":"fixture-access-token personal content"}`, 200, 1},
		{"missing code", `{"data":{"username":"example"}}`, 200, 1},
		{"transient", `{"code":50000,"message":"private"}`, 503, 3},
		{"invalid", `private response`, 200, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			calls := 0
			c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			})
			c.wait = func(context.Context, time.Duration) error { return nil }
			_, err := c.Account(t.Context(), "account-example")
			require.Error(t, err)
			assertions.NotContains(err.Error(), "fixture-access-token")
			assertions.NotContains(err.Error(), "private")
			assertions.Equal(tc.calls, calls)
		})
	}
}

func TestLimiterPersistsPacingAndCooldown(t *testing.T) {
	requirements := require.New(t)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "rate.json")
	l := NewLimiter("")
	l.now = func() time.Time { return now }
	var waited time.Duration
	l.wait = func(_ context.Context, d time.Duration) error { waited += d; now = now.Add(d); return nil }
	for range 601 {
		requirements.NoError(l.Wait(t.Context()))
	}
	assert.GreaterOrEqual(t, waited, time.Minute)
	l = NewLimiter(path)
	l.now = func() time.Time { return now }
	l.wait = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
	requirements.NoError(l.Wait(t.Context()))
	shared := NewLimiter(path)
	shared.now, shared.wait = l.now, l.wait
	requirements.NoError(shared.Wait(t.Context()))
	requirements.NoError(l.Block(t.Context()))
	restarted := NewLimiter(path)
	restarted.now = l.now
	requirements.ErrorIs(restarted.Wait(t.Context()), ErrRateLimited)
	now = now.Add(5 * time.Minute)
	requirements.NoError(restarted.Wait(t.Context()))
}

func TestClientThrottleDoesNotRetryInsideLockout(t *testing.T) {
	requirements := require.New(t)
	calls := 0
	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = fmt.Fprint(w, `{"code":40100,"message":"limited"}`)
	})
	_, err := c.Account(t.Context(), "account-example")
	requirements.ErrorIs(err, ErrRateLimited)
	_, err = c.Account(t.Context(), "account-example")
	requirements.ErrorIs(err, ErrRateLimited)
	assert.Equal(t, 1, calls)
}

func TestClientCancellation(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, `{"code":0,"data":{}}`) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.Account(ctx, "account-example")
	require.ErrorIs(t, err, context.Canceled)
}
