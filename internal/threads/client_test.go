package threads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsClientEndpointContracts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	common := "id,text,timestamp,username,permalink,media_type,media_url,shortcode,children,is_quote_post,quoted_post,has_replies"
	replies := common + ",root_post,replied_to,is_reply,is_reply_owned_by_me"
	var calls atomic.Int32
	c, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("Bearer "+testToken, r.Header.Get("Authorization"))
		q := r.URL.Query()
		assert.Empty(q.Get("access_token"))
		switch r.URL.Path {
		case "/v1.0/me":
			assert.Equal(url.Values{"fields": {"id,username,name"}}, q)
			respond(w, 200, `{"id":"`+testID+`","username":"example_owner","name":"Example Owner"}`)
			return
		case "/v1.0/me/threads", "/v1.0/me/replies":
			fields := common + ",owner,reposted_post"
			if strings.HasSuffix(r.URL.Path, "replies") {
				fields = replies
			}
			assert.Equal(url.Values{"fields": {fields}, "limit": {"100"}, "since": {"1688540400"}, "until": {"1793318400"}, "after": {"opaque + / cursor"}}, q)
		case "/v1.0/123/conversation":
			if !assert.Equal(url.Values{"fields": {replies + ",hide_status"}, "reverse": {"true"}, "after": {"cursor"}}, q) {
				respond(w, 400, `{}`)
				return
			}
		case "/v1.0/" + testID:
			assert.Equal(url.Values{"fields": {replies}}, q)
			respond(w, 200, mediaJSON)
			return
		default:
			assert.Fail("unexpected API route")
			respond(w, 404, `{}`)
			return
		}
		respond(w, 200, `{"data":[`+mediaJSON+`],"paging":{"cursors":{"after":"next"}}}`)
	}, nil)
	owner, err := c.Me(ctx)
	require.NoError(err)
	assert.Equal(testID, owner.ID)
	q := ListQuery{Since: time.Unix(1688540400, 0), Until: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), After: "opaque + / cursor"}
	for _, fetch := range []func(context.Context, ListQuery) (Page, error){c.OwnPosts, c.OwnReplies} {
		p, err := fetch(ctx, q)
		require.NoError(err)
		require.Len(p.Items, 1)
		assert.Equal(testID, p.Items[0].ID)
	}
	p, err := c.Conversation(ctx, "123", "cursor")
	require.NoError(err)
	require.Len(p.Items, 1)
	assert.Nil(p.Items[0].Username)
	assert.Nil(p.Items[0].RootPost, "missing root stays incomplete")
	require.NotNil(p.Items[0].Text)
	assert.Empty(*p.Items[0].Text)
	require.NotNil(p.Items[0].HasReplies)
	assert.False(*p.Items[0].HasReplies)
	assert.True(p.Items[0].Present["text"])
	assert.False(p.Items[0].Present["username"])
	assert.JSONEq(mediaJSON, string(p.Items[0].Raw))
	m, err := c.Media(ctx, testID)
	require.NoError(err)
	assert.Equal(testID, m.ID)
	assert.Equal(int32(5), calls.Load())

	t.Run("invalid input makes no request", func(t *testing.T) {
		assert := assert.New(t)
		before := calls.Load()
		for _, id := range []string{"", "../me", "1/2", "-1", "1e9", "1?access_token=secret", " 123"} {
			_, err := c.Media(ctx, id)
			assert.Error(err)
			_, err = c.Conversation(ctx, id, "")
			assert.Error(err)
		}
		for _, query := range []ListQuery{{}, {Since: q.Until, Until: q.Since}, {Since: q.Since.Add(-time.Second), Until: q.Until}, {Since: q.Since, Until: q.Until.Add(time.Hour)}, {Since: q.Since.Add(time.Nanosecond), Until: q.Since.Add(2 * time.Nanosecond)}} {
			_, err := c.OwnPosts(ctx, query)
			assert.Error(err)
		}
		assert.Equal(before, calls.Load())
	})

	for name, body := range map[string]string{
		"numeric ID":        `{"id":9007199254740993,"timestamp":"2026-10-28T12:00:00Z"}`,
		"bad ID":            `{"id":"1e9","timestamp":"2026-10-28T12:00:00Z"}`,
		"missing ID":        `{"timestamp":"2026-10-28T12:00:00Z"}`,
		"bad timestamp":     `{"id":"1","timestamp":"` + testToken + `"}`,
		"missing timestamp": `{"id":"1"}`,
		"bad reference":     `{"id":"1","timestamp":"2026-10-28T12:00:00Z","root_post":{"id":"../me"}}`,
		"bare reference":    `{"id":"1","timestamp":"2026-10-28T12:00:00Z","root_post":"123"}`,
		"bad child":         `{"id":"1","timestamp":"2026-10-28T12:00:00Z","children":{"data":[{"id":"1.0"}]}}`,
		"duplicate":         `{"id":"1","id":"2","timestamp":"2026-10-28T12:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { respond(w, 200, body) }, nil)
			_, err := client.Media(ctx, "1")
			require.ErrorIs(err, ErrDecodeResponse)
			assert.NotContains(err.Error(), testToken)
		})
	}
	t.Run("null presence", func(t *testing.T) {
		client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			respond(w, 200, `{"id":"1","timestamp":"2026-10-28T12:00:00Z","text":null}`)
		}, nil)
		m, err := client.Media(ctx, "1")
		require.NoError(err)
		assert.Nil(m.Text)
		assert.True(m.Present["text"])
	})
}

func TestThreadsReadRetryDelayAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status, code, want int
	}{
		{"429", 429, 0, 3}, {"500", 500, 0, 3}, {"502", 502, 0, 3}, {"503", 503, 0, 3}, {"504", 504, 0, 3},
		{"501", 501, 0, 1}, {"400", 400, 0, 1}, {"4", 400, 4, 3}, {"17", 400, 17, 3}, {"32", 400, 32, 3}, {"613", 400, 613, 3}, {"190", 400, 190, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var calls atomic.Int32
			c, b, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				respond(w, tc.status, fmt.Sprintf(`{"error":{"code":%d,"message":"%s"}}`, tc.code, testToken))
			}, nil)
			_, err := c.Me(context.Background())
			require.Error(err)
			var api *APIError
			require.ErrorAs(err, &api)
			assert.Equal(tc.code, api.Code)
			assert.Equal(tc.status, api.StatusCode)
			assert.Equal(tc.code == 190, api.CredentialInvalid())
			assert.Equal(int32(tc.want), calls.Load())
			assert.Equal(tc.want, b.Attempts())
			assert.NotContains(fmt.Sprintf("%+v", err), testToken)
		})
	}
	for _, date := range []bool{false, true} {
		t.Run(fmt.Sprintf("90 seconds date=%v", date), func(t *testing.T) {
			assert := assert.New(t)
			var calls atomic.Int32
			var clock *testClock
			c, b, cl := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					h := "90"
					if date {
						h = clock.Now().Add(90 * time.Second).Format(http.TimeFormat)
					}
					w.Header().Set("Retry-After", h)
					respond(w, 429, `{}`)
					return
				}
				respond(w, 200, `{"id":"123"}`)
			}, func(o *ClientOptions) { o.MaxRetryWait = 2 * time.Minute })
			clock = cl
			_, err := c.Me(context.Background())
			require.NoError(t, err)
			assert.Equal(int32(2), calls.Load())
			assert.Equal(2, b.Attempts())
			wantDelay := 90 * time.Second
			if date {
				wantDelay -= 500 * time.Millisecond
			} // HTTP dates have whole-second precision.
			assert.Contains(clock.waits, wantDelay)
		})
	}
	for _, tc := range []struct {
		name             string
		header           string
		maxWait, elapsed time.Duration
	}{
		{"remaining budget", "90", 2 * time.Minute, 9 * time.Minute},
		{"short wait", "90", time.Second, 0},
		{"run limit", "601", 20 * time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, _, clock := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", tc.header)
				respond(w, 429, `{}`)
			}, func(o *ClientOptions) { o.MaxRetryWait = tc.maxWait })
			clock.advance(tc.elapsed)
			_, err := c.Me(context.Background())
			var pause *PauseError
			require.ErrorAs(t, err, &pause)
			assert.Equal(t, int32(1), calls.Load())
		})
	}
	for _, tc := range []struct {
		name, header, value string
		pause               bool
	}{
		{"app usage", "X-App-Usage", `{"call_count":80,"total_cputime":1,"total_time":2}`, true},
		{"page usage", "X-Page-Usage", `{"call_count":80}`, true},
		{"cpu usage", "X-App-Usage", `{"total_cputime":90}`, true},
		{"business usage", "X-Business-Use-Case-Usage", `{"123":[{"call_count":2,"total_time":80}]}`, true},
		{"below usage", "X-App-Usage", `{"call_count":79}`, false},
		{"unknown header", "X-Future-Usage", `{"call_count":100}`, false},
		{"unknown metric", "X-App-Usage", `{"unknown_metric":100}`, false},
		{"invalid header", "X-App-Usage", `not-json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c, b, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(tc.header, tc.value)
				respond(w, 200, `{"data":[`+mediaJSON+`],"paging":{"cursors":{"after":"next"}}}`)
			}, nil)
			p, err := c.Conversation(context.Background(), "123", "")
			if tc.pause {
				var pause *PauseError
				require.ErrorAs(err, &pause)
			} else {
				require.NoError(err)
			}
			require.Len(p.Items, 1)
			assert.Equal("next", p.After)
			assert.Equal(1, b.Attempts())
		})
	}
	t.Run("redirects never dispatch destination", func(t *testing.T) {
		assert := assert.New(t)
		var dest atomic.Int32
		attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { dest.Add(1) }))
		defer attacker.Close()
		c, b, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, attacker.URL+"?access_token="+testToken, 307)
		}, nil)
		_, err := c.Me(context.Background())
		require.ErrorIs(t, err, ErrRedirect)
		assert.NotContains(err.Error(), testToken)
		assert.Zero(dest.Load())
		assert.Equal(1, b.Attempts())
	})
	t.Run("credential failure takes precedence over usage", func(t *testing.T) {
		c, b, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-App-Usage", `{"call_count":90}`)
			respond(w, 400, `{"error":{"code":190,"message":"`+testToken+`"}}`)
		}, nil)
		_, err := c.Me(context.Background())
		var api *APIError
		require.ErrorAs(t, err, &api)
		assert.True(t, api.CredentialInvalid())
		assert.Equal(t, 1, b.Attempts())
	})
	t.Run("high usage stops a retryable failure", func(t *testing.T) {
		c, b, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-App-Usage", `{"call_count":90}`)
			respond(w, 503, `{"error":{"code":4}}`)
		}, nil)
		_, err := c.Me(context.Background())
		var pause *PauseError
		require.ErrorAs(t, err, &pause)
		assert.Equal(t, 1, b.Attempts())
	})
	t.Run("transport failure", func(t *testing.T) {
		assert := assert.New(t)
		var calls int
		c, b, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {}, func(o *ClientOptions) {
			o.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, &url.Error{Op: "Get", URL: "https://example.com/?access_token=" + testToken, Err: errors.New(testToken)}
			})}
		})
		_, err := c.Me(context.Background())
		require.Error(t, err)
		assert.NotContains(fmt.Sprintf("%+v", err), testToken)
		assert.Equal(1, calls)
		assert.Equal(1, b.Attempts())
		assert.NotContains(fmt.Sprintf("%+v", c), testToken)
	})
	t.Run("cancelled wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var clock = newTestClock()
		budget := NewRequestBudget(BudgetOptions{Now: clock.Now, Wait: func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }})
		c, b, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { assert.Fail(t, "cancelled wait reached transport") }, func(o *ClientOptions) { o.Budget = budget })
		_, err := c.Me(ctx)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, b.Attempts())
		assert.Zero(t, budget.Attempts())
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
