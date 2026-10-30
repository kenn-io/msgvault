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
			require.NoError(t, r.ParseForm())
			assert.Equal(t, f.draft.ReplyToSourceMessageID, r.PostForm.Get("reply_to_id"))
			assert.Equal(t, f.draft.Body, r.PostForm.Get("text"))
			id := 400 + f.creates.Add(1) - 1
			if f.createMode == "drop" {
				conn, _, err := w.(http.Hijacker).Hijack()
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				return
			}
			respond(w, 200, fmt.Sprintf(`{"id":"%d"}`, id))
		case r.URL.Path == "/v1.0/456/threads_publish":
			n := f.publishes.Add(1)
			require.NoError(t, r.ParseForm())
			f.publishedContainers = append(f.publishedContainers, r.PostForm.Get("creation_id"))
			switch f.publishMode {
			case "drop":
				conn, _, err := w.(http.Hijacker).Hijack()
				require.NoError(t, err)
				require.NoError(t, conn.Close())
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
