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
			require.NoError(r.ParseForm())
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
			require.NoError(r.ParseForm())
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
	media, err := client.PublishReply(t.Context(), "456", container)
	require.NoError(err)
	assert.Equal(testID, media)
	assert.Equal(4, budget.Attempts())
	assert.Len(calls, 4)
}
func TestThreadsPublishProtocolBodyBytes(t *testing.T) {
	assert := assert.New(t)
	for _, tc := range []struct {
		body  string
		valid bool
	}{{strings.Repeat("a", 499), true}, {strings.Repeat("a", 500), true}, {strings.Repeat("a", 501), false}, {strings.Repeat("é", 250), true}, {strings.Repeat("é", 251), false}, {" \n\t", false}, {"a\x00b", false}, {string([]byte{0xff}), false}} {
		err := ValidateReplyBody(tc.body)
		if tc.valid {
			assert.NoError(err)
		} else {
			assert.ErrorIs(err, ErrInvalidReplyBody)
		}
	}
	var calls atomic.Int32
	client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); respond(w, 200, `{"id":"400"}`) }, nil)
	_, err := client.CreateReply(t.Context(), "100", strings.Repeat("a", 501))
	assert.ErrorIs(err, ErrInvalidReplyBody)
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
					_, err = client.PublishReply(t.Context(), "456", "400")
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
	_, err = client.PublishReply(t.Context(), "456", "400")
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
				return client.PublishReply(t.Context(), "456", "400")
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
