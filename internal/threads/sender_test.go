package threads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type sendFixture struct {
	st                        *store.Store
	writer                    *Writer
	draft                     store.ChatDraft
	rootID                    int64
	api                       *Client
	clock                     *testClock
	options                   SenderOptions
	mu                        sync.Mutex
	mediaStatus               map[string]int
	mediaRaw                  map[string]string
	recoveryReplies           []string
	recoveryListCalls         int
	createErrors              []APIError
	statusErrors              []APIError
	statusRetryAfter          func(time.Time) string
	statusUsage               string
	statuses                  []string
	statusTimes               []time.Time
	quotaUsage                int64
	publishMode               string
	createMode                string
	publishedContainers       []string
	creates, publishes, opens atomic.Int32
}

func newSendFixture(t *testing.T) *sendFixture {
	t.Helper()
	w, st := writerFixture(t)
	root := applyWriter(t, w, writerMedia("100", "Owned post"), ItemOrigin{Kind: "own_posts"})
	target, err := st.GetMessageRelationTarget(root)
	require.NoError(t, err)
	draft, err := st.CreateChatDraftContext(t.Context(), target.ConversationID, root, "reviewed reply", func(string, string) error { return nil })
	require.NoError(t, err)
	f := &sendFixture{st: st, writer: w, draft: draft, rootID: root, clock: newTestClock(), statuses: []string{"FINISHED"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1.0/me/threads_publishing_limit":
			assert.Equal(t, "reply_quota_usage,reply_config", r.URL.Query().Get("fields"))
			respond(w, 200, fmt.Sprintf(`{"data":[{"reply_quota_usage":%d,"reply_config":{"quota_total":1000,"quota_duration":86400}}]}`, f.quotaUsage))
		case r.URL.Path == "/v1.0/me/replies":
			f.recoveryListCalls++
			if r.URL.Query().Get("after") != "" {
				respond(w, 200, `{"data":[]}`)
			} else {
				respond(w, 200, `{"data":[`+strings.Join(f.recoveryReplies, ",")+`],"paging":{"cursors":{"after":"tail"}}}`)
			}
		case r.URL.Path == "/v1.0/me/threads":
			if !assert.NoError(t, r.ParseForm()) {
				return
			}
			assert.Equal(t, f.draft.ReplyToSourceMessageID, r.PostForm.Get("reply_to_id"))
			assert.Equal(t, f.draft.Body, r.PostForm.Get("text"))
			id := 400 + f.creates.Add(1) - 1
			if len(f.createErrors) > 0 {
				failure := f.createErrors[0]
				f.createErrors = f.createErrors[1:]
				respond(w, failure.StatusCode, fmt.Sprintf(`{"error":{"code":%d}}`, failure.Code))
				return
			}
			if f.createMode == "drop" {
				hijacker, ok := w.(http.Hijacker)
				if !assert.True(t, ok) {
					return
				}
				conn, _, err := hijacker.Hijack()
				if !assert.NoError(t, err) {
					return
				}
				if !assert.NoError(t, conn.Close()) {
					return
				}
				return
			}
			respond(w, 200, fmt.Sprintf(`{"id":"%d"}`, id))
		case r.URL.Path == "/v1.0/456/threads_publish":
			n := f.publishes.Add(1)
			if !assert.NoError(t, r.ParseForm()) {
				return
			}
			f.publishedContainers = append(f.publishedContainers, r.PostForm.Get("creation_id"))
			switch f.publishMode {
			case "drop":
				hijacker, ok := w.(http.Hijacker)
				if !assert.True(t, ok) {
					return
				}
				conn, _, err := hijacker.Hijack()
				if !assert.NoError(t, err) {
					return
				}
				if !assert.NoError(t, conn.Close()) {
					return
				}
			case "500":
				respond(w, 500, `{"error":{"code":1}}`)
			default:
				respond(w, 200, fmt.Sprintf(`{"id":"%d"}`, 500+n-1))
			}
		case strings.HasPrefix(r.URL.Path, "/v1.0/"):
			id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/v1.0/"))
			if raw, ok := f.mediaRaw[strconv.Itoa(id)]; err == nil && ok {
				status := http.StatusOK
				if configured := f.mediaStatus[strconv.Itoa(id)]; configured != 0 {
					status = configured
				}
				respond(w, status, raw)
				return
			}
			if err != nil || id < 400 || id >= 500 {
				http.NotFound(w, r)
				return
			}
			f.statusTimes = append(f.statusTimes, f.clock.Now())
			if len(f.statusErrors) > 0 {
				if f.statusRetryAfter != nil {
					w.Header().Set("Retry-After", f.statusRetryAfter(f.clock.Now()))
				}
				if f.statusUsage != "" {
					w.Header().Set("X-App-Usage", f.statusUsage)
				}
				failure := f.statusErrors[0]
				f.statusErrors = f.statusErrors[1:]
				respond(w, failure.StatusCode, fmt.Sprintf(`{"error":{"code":%d}}`, failure.Code))
				return
			}
			status := f.statuses[0]
			if len(f.statuses) > 1 {
				f.statuses = f.statuses[1:]
			}
			respond(w, 200, fmt.Sprintf(`{"id":"%d","status":%q}`, id, status))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.api, err = NewClient(ClientOptions{BaseURL: server.URL + "/v1.0", AccessToken: testToken, HTTPClient: server.Client(), Budget: f.clock.budget(1000)})
	require.NoError(t, err)
	f.options = SenderOptions{Authorize: func(context.Context) error { return nil }, LoadPolicy: func(context.Context, int64) (SendPolicy, error) {
		return SendPolicy{Enabled: true, SourceIdentifier: writerSource}, nil
	}, OpenAPI: func(context.Context, int64) (PublishAPI, []string, error) {
		f.opens.Add(1)
		return f.api, []string{"threads_basic", "threads_read_replies", "threads_content_publish", "threads_manage_replies"}, nil
	}, Now: f.clock.Now, Wait: f.clock.Wait}
	return f
}
func (f *sendFixture) request() SendRequest {
	return SendRequest{DraftID: f.draft.DraftID, Revision: f.draft.Revision}
}
func (f *sendFixture) setStatuses(statuses ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = statuses
}
func (f *sendFixture) setMode(mode string) { f.mu.Lock(); defer f.mu.Unlock(); f.publishMode = mode }

func TestThreadsPublishProtocolSender(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	f.setStatuses("IN_PROGRESS", "IN_PROGRESS", "FINISHED")
	started := f.clock.Now()
	result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.NoError(err)
	assert.Equal("500", result.MediaID)
	assert.Equal("sent", result.RemoteStatus)
	assert.Contains([]string{"sent_archive_pending", "complete"}, result.Status)
	assert.Equal(int32(1), f.creates.Load())
	assert.Equal(int32(1), f.publishes.Load())
	f.mu.Lock()
	times := append([]time.Time(nil), f.statusTimes...)
	f.mu.Unlock()
	require.Len(times, 3)
	assert.GreaterOrEqual(times[0].Sub(started), 30*time.Second)
	for n := 1; n < len(times); n++ {
		assert.GreaterOrEqual(times[n].Sub(times[n-1]), time.Minute)
	}
	receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	assert.Equal("500", receipt.MediaID)
	assert.NotNil(receipt.PublishDispatchedAt)
	// A known outcome replays without a credential lookup or quota request.
	f.options.OpenAPI = func(context.Context, int64) (PublishAPI, []string, error) {
		require.Fail("known outcome requested credentials")
		return nil, nil, errors.New("expired")
	}
	again, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.NoError(err)
	assert.Equal("500", again.MediaID)
	assert.Equal(int32(1), f.publishes.Load())
}
func TestThreadsPublishProtocolPreflightBeforeCredentials(t *testing.T) {
	for _, kind := range []string{"owner", "optout", "binding", "revision", "external-root", "oversized", "blank", "missing-parent", "missing-root"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newSendFixture(t)
			request := f.request()
			switch kind {
			case "owner":
				f.options.Authorize = func(context.Context) error { return errors.New("owner required") }
			case "optout":
				f.options.LoadPolicy = func(context.Context, int64) (SendPolicy, error) {
					return SendPolicy{SourceIdentifier: writerSource}, nil
				}
			case "binding":
				f.options.LoadPolicy = func(context.Context, int64) (SendPolicy, error) {
					return SendPolicy{Enabled: true, SourceIdentifier: "threads:123:789"}, nil
				}
			case "revision":
				request.Revision++
			case "external-root":
				require.NoError(f.st.SetMessageMetadataContext(t.Context(), f.rootID, sql.NullString{String: `{"root_id":"100","root_owned":false}`, Valid: true}))
			case "oversized", "blank":
				body := strings.Repeat("a", 501)
				if kind == "blank" {
					body = " \n\t"
				}
				draft, err := f.st.UpdateChatDraftContext(t.Context(), f.draft.DraftID, 1, body)
				require.NoError(err)
				f.draft = draft
				request = f.request()
			case "missing-parent", "missing-root":
				parent := applyWriter(t, f.writer, writerReply("101", "100", "100"), ItemOrigin{Kind: "conversation", RootID: "100"})
				draft, err := f.st.CreateChatDraftContext(t.Context(), f.draft.ConversationID, parent, "reviewed reply", func(string, string) error { return nil })
				require.NoError(err)
				f.draft = draft
				request = f.request()
				removed := "101"
				if kind == "missing-root" {
					removed = "100"
				}
				require.NoError(f.st.MarkMessageDeletedContext(t.Context(), f.writer.sourceID, removed))
			}
			_, err := NewSender(f.st, f.options).Send(t.Context(), request)
			require.Error(err)
			assert.Zero(f.opens.Load())
			assert.Zero(f.creates.Load())
			assert.Zero(f.publishes.Load())
			draft, err := f.st.GetChatDraftContext(t.Context(), f.draft.DraftID)
			require.NoError(err)
			assert.Equal(f.draft.Body, draft.Body)
		})
	}
}
func TestThreadsPublishProtocolScopesAndQuota(t *testing.T) {
	for _, kind := range []string{"scopes", "quota"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			f := newSendFixture(t)
			if kind == "scopes" {
				f.options.OpenAPI = func(context.Context, int64) (PublishAPI, []string, error) {
					return f.api, []string{"threads_basic", "threads_read_replies"}, nil
				}
			} else {
				f.quotaUsage = 1000
			}
			result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
			require.Error(t, err)
			assert.Contains([]string{"reply_not_permitted", "rate_limited"}, result.Status)
			assert.Zero(f.creates.Load())
			assert.Zero(f.publishes.Load())
			_, err = f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
			assert.ErrorIs(err, store.ErrThreadsReplyNotFound)
		})
	}
}

func TestThreadsPublishProtocolQuotaFailureCategories(t *testing.T) {
	for _, tc := range []struct {
		status, code int
		want         string
	}{{500, 1, "send_failed"}, {400, 190, "reply_not_permitted"}, {429, 4, "rate_limited"}} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			assert := assert.New(t)
			f := newSendFixture(t)
			api, _, clock := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				respond(w, tc.status, fmt.Sprintf(`{"error":{"code":%d}}`, tc.code))
			}, nil)
			f.api = api
			f.options.Now = clock.Now
			f.options.Wait = clock.Wait
			result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
			require.Error(t, err)
			assert.Equal(tc.want, result.Status)
			assert.Equal("unpublished", result.RemoteStatus)
			assert.Zero(f.creates.Load())
			assert.Zero(f.publishes.Load())
		})
	}
}

// A real retryable HTTP response must yield to the persisted poll reservation.
func TestThreadsContainerStatusRetryUsesDurableReservation(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newSendFixture(t)
			f.statusErrors = []APIError{{StatusCode: status, Code: 1}}
			_, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
			require.Error(err, "retryable polling response must yield instead of issuing hidden requests")
			assert.Zero(f.publishes.Load())
			f.mu.Lock()
			times := append([]time.Time(nil), f.statusTimes...)
			f.mu.Unlock()
			require.Len(times, 1, "one durable reservation permits exactly one status request")
			receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
			require.NoError(err)
			require.NotNil(receipt.NextPollAt)
			assert.GreaterOrEqual(receipt.NextPollAt.Sub(times[0]), time.Minute)
			result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
			require.NoError(err)
			assert.Equal("sent", result.RemoteStatus)
			f.mu.Lock()
			times = append([]time.Time(nil), f.statusTimes...)
			f.mu.Unlock()
			require.Len(times, 2)
			assert.GreaterOrEqual(times[1].Sub(times[0]), time.Minute, "a new sender must honor the saved reservation")
			assert.Equal(int32(1), f.creates.Load())
			assert.Equal(int32(1), f.publishes.Load())
		})
	}
}

func TestThreadsRetryableCreationCanResumeSameRequest(t *testing.T) {
	for _, failure := range []APIError{
		{StatusCode: http.StatusTooManyRequests},
		{StatusCode: http.StatusBadRequest, Code: 4},
		{StatusCode: http.StatusBadRequest, Code: 17},
		{StatusCode: http.StatusBadRequest, Code: 32},
		{StatusCode: http.StatusBadRequest, Code: 613},
		{StatusCode: http.StatusForbidden, Code: 4},
		{StatusCode: http.StatusServiceUnavailable, Code: 1},
	} {
		t.Run(fmt.Sprintf("%d-code%d", failure.StatusCode, failure.Code), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newSendFixture(t)
			f.createErrors = []APIError{failure}
			request := f.request()
			_, err := NewSender(f.st, f.options).Send(t.Context(), request)
			require.Error(err)
			assert.Equal(int32(1), f.creates.Load(), "POST itself must never retry automatically")
			assert.Zero(f.publishes.Load())
			receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
			require.NoError(err)
			assert.Equal("claimed", receipt.State, "throttling is resumable, not a terminal rejection")
			assert.Empty(receipt.ContainerID)
			assert.Nil(receipt.PublishDispatchedAt)
			result, err := NewSender(f.st, f.options).Send(t.Context(), request)
			require.NoError(err)
			assert.Equal("sent", result.RemoteStatus)
			assert.Equal(int32(2), f.creates.Load())
			assert.Equal(int32(1), f.publishes.Load())
			again, err := NewSender(f.st, f.options).Send(t.Context(), request)
			require.NoError(err)
			assert.Equal(result.MediaID, again.MediaID)
			assert.Equal(int32(1), f.publishes.Load(), "known outcome must not publish again")
		})
	}
}

func TestThreadsPollingProviderCooldownSurvivesResume(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		for _, tc := range []struct {
			name      string
			header    func(time.Time) string
			cooldown  time.Duration
			highUsage bool
		}{
			{name: "seconds", header: func(time.Time) string { return "300" }, cooldown: 5 * time.Minute},
			{name: "http-date", header: func(at time.Time) string { return at.Add(5 * time.Minute).UTC().Format(http.TimeFormat) }, cooldown: 5 * time.Minute},
			{name: "beyond-run", header: func(time.Time) string { return "900" }, cooldown: 15 * time.Minute},
			{name: "short", header: func(time.Time) string { return "10" }, cooldown: time.Minute},
			{name: "malformed", header: func(time.Time) string { return "not-a-delay" }, cooldown: time.Minute},
			{name: "high-usage", header: func(time.Time) string { return "300" }, cooldown: 5 * time.Minute, highUsage: true},
		} {
			t.Run(strconv.Itoa(status)+"/"+tc.name, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				f := newSendFixture(t)
				f.statusErrors = []APIError{{StatusCode: status, Code: 1}}
				f.statusRetryAfter = tc.header
				if tc.highUsage {
					f.statusUsage = `{"call_count":80}`
				}
				_, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
				require.Error(err)
				f.mu.Lock()
				times := append([]time.Time(nil), f.statusTimes...)
				f.mu.Unlock()
				require.Len(times, 1, "status must make only one HTTP attempt")
				receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
				require.NoError(err)
				require.NotNil(receipt.NextPollAt)
				assert.GreaterOrEqual(receipt.NextPollAt.Sub(times[0]), tc.cooldown, "the database must retain the provider's full cooldown")
				var result SendResult
				for range 5 {
					// Each daemon invocation gets a fresh request budget; the receipt must
					// retain the cooldown even when it spans several five-minute send runs.
					f.api, err = NewClient(ClientOptions{BaseURL: f.api.base.String(), AccessToken: testToken, Budget: f.clock.budget(1000)})
					require.NoError(err)
					result, err = NewSender(f.st, f.options).Send(t.Context(), f.request())
					require.NoError(err)
					if result.RemoteStatus == "sent" {
						break
					}
					f.mu.Lock()
					count := len(f.statusTimes)
					f.mu.Unlock()
					assert.Equal(1, count, "a bounded invocation must yield without polling before cooldown")
				}
				assert.Equal("sent", result.RemoteStatus)
				f.mu.Lock()
				times = append([]time.Time(nil), f.statusTimes...)
				f.mu.Unlock()
				require.Len(times, 2)
				assert.GreaterOrEqual(times[1].Sub(times[0]), tc.cooldown, "a resumed sender must never poll before the provider deadline")
				assert.Equal(int32(1), f.creates.Load())
				assert.Equal(int32(1), f.publishes.Load())
			})
		}
	}
}

// This wrapper delegates real HTTP and cancels at the received-response boundary.
type cancelAfterStatusAPI struct {
	PublishAPI

	cancel context.CancelFunc
}

func (a cancelAfterStatusAPI) ContainerStatus(ctx context.Context, id string) (string, error) {
	status, err := a.PublishAPI.ContainerStatus(ctx, id)
	a.cancel()
	return status, err
}
func TestThreadsPollingCooldownCheckpointSurvivesCancellation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newSendFixture(t)
	f.statusErrors = []APIError{{StatusCode: http.StatusTooManyRequests, Code: 1}}
	f.statusRetryAfter = func(time.Time) string { return "300" }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.options.OpenAPI = func(context.Context, int64) (PublishAPI, []string, error) {
		return cancelAfterStatusAPI{PublishAPI: f.api, cancel: cancel}, []string{"threads_basic", "threads_content_publish", "threads_manage_replies"}, nil
	}
	_, err := NewSender(f.st, f.options).Send(ctx, f.request())
	require.Error(err)
	require.ErrorIs(ctx.Err(), context.Canceled)
	f.mu.Lock()
	times := append([]time.Time(nil), f.statusTimes...)
	f.mu.Unlock()
	require.Len(times, 1)
	receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	require.NotNil(receipt.NextPollAt)
	assert.GreaterOrEqual(receipt.NextPollAt.Sub(times[0]), 5*time.Minute, "cancellation must not erase an already received provider cooldown")
	assert.Zero(f.publishes.Load())
}
