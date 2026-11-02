package threads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsPublishProtocolHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	calls := []string{}
	client, budget, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		assert.Equal("Bearer "+testToken, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1.0/me/threads_publishing_limit":
			assert.Equal("reply_quota_usage,reply_config", r.URL.Query().Get("fields"))
			respond(w, 200, `{"data":[{"reply_quota_usage":17,"reply_config":{"quota_total":1000,"quota_duration":86400}}]}`)
		case "/v1.0/me/threads":
			if !assert.NoError(r.ParseForm()) {
				return
			}
			assert.Equal("TEXT", r.PostForm.Get("media_type"))
			assert.Equal("100", r.PostForm.Get("reply_to_id"))
			assert.Equal("reviewed & exact", r.PostForm.Get("text"))
			assert.Empty(r.PostForm.Get("auto_publish_text"))
			assert.Empty(r.URL.RawQuery)
			respond(w, 200, `{"id":"400"}`)
		case "/v1.0/400":
			assert.Equal("status,error_message", r.URL.Query().Get("fields"))
			respond(w, 200, `{"id":"400","status":"FINISHED"}`)
		case "/v1.0/456/threads_publish":
			if !assert.NoError(r.ParseForm()) {
				return
			}
			assert.Equal("400", r.PostForm.Get("creation_id"))
			assert.Empty(r.Header.Get("Idempotency-Key"))
			assert.Empty(r.Header.Get("X-Idempotency-Key"))
			respond(w, 200, `{"id":"`+testID+`"}`)
		default:
			assert.Fail("unexpected endpoint", r.URL.Path)
			http.NotFound(w, r)
		}
	}, nil)
	quota, err := client.ReplyQuota(t.Context())
	require.NoError(err)
	assert.Equal(ReplyQuota{Usage: 17, Total: 1000, Duration: 86400}, quota)
	container, err := client.CreateReply(t.Context(), "100", "reviewed & exact")
	require.NoError(err)
	assert.Equal("400", container)
	status, err := client.ContainerStatus(t.Context(), container)
	require.NoError(err)
	assert.Equal("FINISHED", status)
	media, err := client.PublishReply(t.Context(), "456", container, nil)
	require.NoError(err)
	assert.Equal(testID, media)
	assert.Equal(4, budget.Attempts())
	assert.Len(calls, 4)
}
func TestThreadsPublishProtocolBodyBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, tc := range []struct {
		body  string
		valid bool
	}{{strings.Repeat("a", 499), true}, {strings.Repeat("a", 500), true}, {strings.Repeat("a", 501), false}, {strings.Repeat("é", 250), true}, {strings.Repeat("é", 251), false}, {" \n\t", false}, {"a\x00b", false}, {string([]byte{0xff}), false}} {
		err := ValidateReplyBody(tc.body)
		if tc.valid {
			require.NoError(err)
		} else {
			require.ErrorIs(err, ErrInvalidReplyBody)
		}
	}
	var calls atomic.Int32
	client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); respond(w, 200, `{"id":"400"}`) }, nil)
	_, err := client.CreateReply(t.Context(), "100", strings.Repeat("a", 501))
	require.ErrorIs(err, ErrInvalidReplyBody)
	assert.Zero(calls.Load())
}
func TestThreadsPublishProtocolNeverRetriesPosts(t *testing.T) {
	for _, method := range []string{"create", "publish"} {
		for _, status := range []int{500, 429, 301, 302, 307, 308} {
			t.Run(method+strconv.Itoa(status), func(t *testing.T) {
				assert := assert.New(t)
				var targetCalls, originCalls atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); respond(w, 200, `{"id":"500"}`) }))
				defer target.Close()
				client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					originCalls.Add(1)
					w.Header().Set("Location", target.URL)
					respond(w, status, `{"error":{"code":1,"message":"synthetic-secret-body"}}`)
				}, nil)
				var err error
				if method == "create" {
					_, err = client.CreateReply(t.Context(), "100", "reviewed")
				} else {
					_, err = client.PublishReply(t.Context(), "456", "400", nil)
				}
				require.Error(t, err)
				assert.NotContains(fmt.Sprint(err), "synthetic-secret-body")
				assert.Equal(int32(1), originCalls.Load())
				assert.Zero(targetCalls.Load())
			})
		}
	}
}
func TestThreadsPublishProtocolQuotaFailsClosed(t *testing.T) {
	for _, body := range []string{`{"data":[]}`, `{"data":[{"reply_config":{"quota_total":1000,"quota_duration":86400}}]}`, `{"data":[{"reply_quota_usage":-1,"reply_config":{"quota_total":1000,"quota_duration":86400}}]}`, `{"data":[{"reply_quota_usage":0,"reply_config":{"quota_total":0,"quota_duration":86400}}]}`} {
		client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { respond(w, 200, body) }, nil)
		_, err := client.ReplyQuota(context.Background())
		assert.Error(t, err)
	}
}

func TestThreadsPublishProtocolPreservesBudgetPause(t *testing.T) {
	assert := assert.New(t)
	clock := newTestClock()
	var calls atomic.Int32
	client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		respond(w, 200, `{"data":[{"reply_quota_usage":0,"reply_config":{"quota_total":1000,"quota_duration":86400}}]}`)
	}, func(opts *ClientOptions) { opts.Budget = clock.budget(1) })
	_, err := client.ReplyQuota(t.Context())
	require.NoError(t, err)
	_, err = client.CreateReply(t.Context(), "100", "reviewed")
	_, paused := errors.AsType[*PauseError](err)
	assert.True(paused, "%v", err)
	_, err = client.PublishReply(t.Context(), "456", "400", nil)
	_, paused = errors.AsType[*PauseError](err)
	assert.True(paused, "%v", err)
	assert.Equal(int32(1), calls.Load())
}

func TestThreadsPublishProtocolHighUsageRetainsReturnedID(t *testing.T) {
	for _, method := range []string{"create", "publish"} {
		t.Run(method, func(t *testing.T) {
			assert := assert.New(t)
			var calls atomic.Int32
			client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("X-App-Usage", `{"call_count":80}`)
				respond(w, 200, `{"id":"`+testID+`"}`)
			}, nil)
			call := func() (string, error) {
				if method == "create" {
					return client.CreateReply(t.Context(), "100", "reviewed")
				}
				return client.PublishReply(t.Context(), "456", "400", nil)
			}
			id, err := call()
			assert.Equal(testID, id)
			_, paused := errors.AsType[*PauseError](err)
			assert.True(paused)
			id, err = call()
			assert.Empty(id)
			_, paused = errors.AsType[*PauseError](err)
			assert.True(paused)
			assert.Equal(int32(1), calls.Load())
		})
	}
}

func TestThreadsPublishAdmissionHoldsSourceSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		started := time.Now()
		limiter := NewRateLimiter(nil, nil)
		budget := NewRequestBudget(BudgetOptions{MaxDuration: time.Second, Limiter: limiter})
		type requestStart struct {
			path string
			at   time.Time
		}
		requests := make(chan requestStart, 2)
		// A single-dispatch transport supplies synthetic provider responses while
		// synctest controls limiter waits and time spent committing the barrier.
		httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests <- requestStart{r.URL.Path, time.Now()}
			response := httptest.NewRecorder()
			if r.URL.Path == "/v1.0/456/threads_publish" {
				respond(response, 200, `{"id":"500"}`)
			} else {
				respond(response, 200, `{"data":[{"reply_quota_usage":0,"reply_config":{"quota_total":1000,"quota_duration":86400}}]}`)
			}
			return response.Result(), nil
		})}
		client, err := NewClient(ClientOptions{HTTPClient: httpClient, BaseURL: "https://example.com/v1.0", AccessToken: testToken, Budget: budget})
		require.NoError(err)
		other, err := NewClient(ClientOptions{HTTPClient: httpClient, BaseURL: "https://example.com/v1.0", AccessToken: testToken, Budget: NewRequestBudget(BudgetOptions{Limiter: limiter})})
		require.NoError(err)
		otherDone := make(chan error, 1)
		media, err := client.PublishReply(t.Context(), "456", "400", func(context.Context) error {
			go func() {
				_, err := other.ReplyQuota(t.Context())
				otherDone <- err
			}()
			synctest.Wait()
			time.Sleep(250 * time.Millisecond)
			return nil
		})
		require.NoError(err)
		assert.Equal("500", media)
		require.NoError(<-otherDone)
		first, second := <-requests, <-requests
		assert.Equal("/v1.0/456/threads_publish", first.path)
		assert.Equal(started.Add(750*time.Millisecond), first.at)
		assert.Equal("/v1.0/me/threads_publishing_limit", second.path)
		assert.GreaterOrEqual(second.at.Sub(first.at), 500*time.Millisecond, "a shared-budget request cannot steal the publish slot during the barrier")
		assert.Equal(1, budget.Attempts(), "publish admission and dispatch consume one slot")
	})
}

func TestThreadsPublishAdmissionCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var calls atomic.Int32
		budget := NewRequestBudget(BudgetOptions{})
		client, err := NewClient(ClientOptions{BaseURL: "https://example.com/v1.0", AccessToken: testToken, Budget: budget, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("unexpected dispatch")
		})}})
		require.NoError(err)
		ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		defer cancel()
		barrier := false
		_, err = client.PublishReply(ctx, "456", "400", func(context.Context) error {
			barrier = true
			return nil
		})
		require.ErrorIs(err, context.DeadlineExceeded)
		assert.False(barrier, "a caller deadline during admission must not commit a barrier")
		assert.Zero(calls.Load())
		assert.Zero(budget.Attempts())
	})
}

func TestThreadsPublishAdmissionDeadlineAfterBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var dispatches atomic.Int32
		budget := NewRequestBudget(BudgetOptions{MaxDuration: time.Second})
		client, err := NewClient(ClientOptions{BaseURL: "https://example.com/v1.0", AccessToken: testToken, Budget: budget, HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			// A transport can reject an expired context before sending bytes.
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
			dispatches.Add(1)
			return nil, errors.New("unexpected dispatch")
		})}})
		require.NoError(err)
		barrier := false
		_, err = client.PublishReply(t.Context(), "456", "400", func(context.Context) error {
			barrier = true
			// Model expiry after commit, e.g. during the durable-state readback.
			time.Sleep(time.Second)
			return nil
		})
		var pause *PauseError
		require.ErrorAs(err, &pause)
		assert.Equal("time_limit", pause.Reason)
		assert.True(barrier, "a lease cannot make commit and HTTP dispatch atomic")
		assert.Zero(dispatches.Load())
		assert.Equal(1, budget.Attempts(), "an admitted publish consumes one slot without reacquiring")
	})
}
