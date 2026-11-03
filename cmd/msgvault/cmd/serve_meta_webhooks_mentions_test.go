package cmd

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMentionWebhookEndToEnd(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	cfg := webhookTestConfig(t)
	profile := config.InstagramCommentsSource{Identifier: "example", AppID: "9", PageID: "100", InstagramAccountID: "200", TokenEnv: "EXAMPLE_IG_TOKEN", Mentions: true, Enabled: true}
	cfg.InstagramComments = []config.InstagramCommentsSource{profile}
	t.Setenv(profile.TokenEnv, "synthetic-token")
	var requests atomic.Int32
	var own atomic.Bool
	var refused atomic.Int32
	var invalidCommentTime atomic.Bool
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, "Bearer synthetic-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200"}}`)
		case "/v26.0/200":
			if status := refused.Load(); status != 0 {
				w.WriteHeader(int(status))
				return
			}
			media := `{"id":"300","caption":"Example mention","media_type":"IMAGE"}`
			if own.Load() {
				media = `{"id":"300","owner":{"id":"200"}}`
			}
			if strings.HasPrefix(r.URL.Query().Get("fields"), "mentioned_comment") {
				timestamp := ""
				if invalidCommentTime.Load() {
					timestamp = `,"timestamp":"invalid"`
				}
				_, _ = fmt.Fprintf(w, `{"mentioned_comment":{"id":"400","text":"Example comment"%s,"media":%s}}`, timestamp, media)
			} else {
				_, _ = fmt.Fprintf(w, `{"mentioned_media":%s}`, media)
			}
		default:
			assert.Fail(t, "unexpected fixture route", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	destination, err := url.Parse(server.URL)
	requirements.NoError(err)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "graph.facebook.com", r.URL.Host)
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = destination.Scheme, destination.Host
		return original.RoundTrip(clone)
	})
	st := testutil.NewSQLiteTestStore(t)
	src, err := registerInstagramMentionsProfile(t.Context(), st, profile)
	requirements.NoError(err)
	second := profile
	second.AppID = "10"
	cfg.InstagramComments = append(cfg.InstagramComments, second)
	other, err := registerInstagramMentionsProfile(t.Context(), st, second)
	requirements.NoError(err)
	now := time.Now()
	spool, err := metawebhook.OpenSpool(t.TempDir(), 1<<20, func() time.Time { return now })
	requirements.NoError(err)
	reg := metawebhook.NewRegistry()
	registerMetaWebhookHandlers(reg, cfg, st)
	_, ok := reg.Lookup("page", "mention")
	assertions.False(ok)
	handler, err := metawebhook.NewHandler(metawebhook.HandlerOptions{Spool: spool, Apps: map[string]metawebhook.AppCredentials{"9": {Secret: []byte("synthetic-secret"), VerifyToken: "example"}, "10": {Secret: []byte("synthetic-secret"), VerifyToken: "example"}, "11": {Secret: []byte("synthetic-secret"), VerifyToken: "example"}}})
	requirements.NoError(err)
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg, OnApplied: func() { refreshes.Add(1) }})
	send := func(app, account, value string) {
		t.Helper()
		body := fmt.Sprintf(`{"object":"instagram","entry":[{"id":%q,"changes":[{"field":"mentions","value":%s}]}]}`, account, value)
		mac := hmac.New(sha256.New, []byte("synthetic-secret"))
		_, _ = mac.Write([]byte(body))
		req := httptest.NewRequest(http.MethodPost, "/meta/webhook/"+app, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		require.Equal(t, http.StatusOK, response.Code)
	}
	count := func(source int64) int {
		t.Helper()
		var n int
		require.NoError(t, st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, source).Scan(&n))
		return n
	}
	send("10", "200", `{"media_id":"300","comment_id":"400"}`)
	sum, err := worker.DrainOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, sum.Applied)
	assertions.Equal(0, count(src.ID))
	assertions.Equal(2, count(other.ID))
	before := requests.Load()
	send("10", "200", `{"media_id":"300","comment_id":"400"}`)
	_, err = worker.DrainOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(before, requests.Load())
	assertions.Equal(2, count(other.ID))
	send("9", "999", `{"media_id":"300"}`)
	sum, err = worker.DrainOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, sum.Held)
	assertions.Equal(before, requests.Load())
	assertions.Zero(count(src.ID))
	assertions.Equal(2, count(other.ID))
	lock, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	requirements.NoError(err)
	send("9", "200", `{"media_id":"300","comment_id":"400"}`)
	sum, err = worker.DrainOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, sum.Retried)
	assertions.Equal(before, requests.Load())
	requirements.NoError(lock.Release())
	now = now.Add(time.Minute)
	due, err := spool.Due(10)
	requirements.NoError(err)
	requirements.Len(due, 1)
	assertions.Zero(due[0].Attempts, "source contention does not consume the retry budget")
	sum, err = worker.DrainOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, sum.Applied)
	assertions.Equal(2, count(src.ID))
	refused.Store(403)
	send("10", "200", `{"media_id":"300"}`)
	sum, err = worker.DrainOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, sum.Retried)
	now = now.Add(time.Hour)
	due, err = spool.Due(10)
	requirements.NoError(err)
	requirements.Len(due, 1)
	assertions.Equal(1, due[0].Attempts, "Graph refusals consume an attempt")
	refused.Store(0)
	h, ok := reg.Lookup("instagram", "mentions")
	requirements.True(ok)
	item := metawebhook.Item{AppID: "9", EntryID: "200", Value: []byte(`{"media_id":"300","comment_id":"400"}`)}
	out := h(t.Context(), "9", []metawebhook.Item{item, item})
	for _, result := range out {
		assertions.Equal(metawebhook.Applied, result.Class)
	}
	assertions.Equal(2, count(src.ID), "handler replay preserves one root and one comment")
	var sameConversation, parentLinked bool
	requirements.NoError(st.DB().QueryRow(`SELECT child.conversation_id=root.conversation_id, child.reply_to_message_id=root.id FROM messages root JOIN messages child ON child.source_id=root.source_id WHERE root.source_id=? AND root.source_message_id='300' AND child.source_message_id='400'`, src.ID).Scan(&sameConversation, &parentLinked))
	assertions.True(sameConversation)
	assertions.True(parentLinked)
	var searchable int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'comment'`).Scan(&searchable))
	assertions.Equal(2, searchable)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	out = h(cancelled, "9", []metawebhook.Item{item})
	assertions.Equal(metawebhook.Retry, out[0].Class)
	assertions.False(out[0].CountAttempt, "shutdown is not a provider failure")
	t.Setenv(profile.TokenEnv, "")
	out = h(t.Context(), "9", []metawebhook.Item{item})
	assertions.Equal(metawebhook.Retry, out[0].Class)
	assertions.Equal("mentions_token_invalid", out[0].ErrorClass)
	assertions.False(out[0].CountAttempt)
	t.Setenv(profile.TokenEnv, "synthetic-token")
	third := profile
	third.Identifier, third.AppID = "example-unregistered", "11"
	cfg.InstagramComments = append(cfg.InstagramComments, third)
	ownItem := item
	ownItem.AppID = "11"
	out = h(t.Context(), "11", []metawebhook.Item{ownItem})
	assertions.Equal(metawebhook.Unrouted, out[0].Class)
	assertions.Equal("mentions_source_missing", out[0].ErrorClass)
	assertions.False(out[0].CountAttempt)
	empty, err := registerInstagramMentionsProfile(t.Context(), st, third)
	requirements.NoError(err)
	own.Store(true)
	out = h(t.Context(), "11", []metawebhook.Item{ownItem})
	assertions.Equal(metawebhook.Ignored, out[0].Class)
	assertions.Zero(count(empty.ID))
	own.Store(false)
	// Finish the earlier provider-refused delivery so this turn contains only
	// the partial mention write, whose committed root must refresh the cache.
	_, err = worker.DrainOnce(t.Context())
	requirements.NoError(err)
	beforeRefresh := refreshes.Load()
	invalidCommentTime.Store(true)
	send("11", "200", `{"media_id":"300","comment_id":"400"}`)
	sum, err = worker.DrainOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, sum.Retried)
	assertions.Equal(1, count(empty.ID), "the root committed before the invalid comment failed")
	assertions.Equal(beforeRefresh+1, refreshes.Load(), "partial writes refresh the archive even while their delivery retries")
	invalidCommentTime.Store(false)
	now = now.Add(time.Hour)
	sum, err = worker.DrainOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(1, sum.Applied)
	assertions.Equal(2, count(empty.ID))
	// The worker holds the operation gate for a handler batch. Bound each
	// mention turn, then leave the backlog retryable without consuming attempts.
	batch := make([]metawebhook.Item, 17)
	for i := range batch {
		batch[i] = item
	}
	before = requests.Load()
	out = h(t.Context(), "9", batch)
	assertions.LessOrEqual(requests.Load()-before, int32(48))
	for _, result := range out[:16] {
		assertions.Equal(metawebhook.Applied, result.Class)
	}
	assertions.Equal(metawebhook.Retry, out[16].Class)
	assertions.Equal("request_budget", out[16].ErrorClass)
	assertions.False(out[16].CountAttempt)
	out = h(t.Context(), "9", batch[16:])
	assertions.Equal(metawebhook.Applied, out[0].Class)
	refused.Store(500)
	out = h(t.Context(), "9", []metawebhook.Item{item, item})
	for _, result := range out {
		assertions.Equal(metawebhook.Retry, result.Class)
		assertions.True(result.CountAttempt)
	}
	refused.Store(0)
	cfg.InstagramComments = cfg.InstagramComments[:2]
	ambiguous := profile
	ambiguous.Identifier, ambiguous.PageID = "example-other-page", "101"
	cfg.InstagramComments = append(cfg.InstagramComments, ambiguous)
	out = h(t.Context(), "9", []metawebhook.Item{item})
	assertions.Equal(metawebhook.Unrouted, out[0].Class)
	assertions.Equal("mentions_profile_ambiguous", out[0].ErrorClass)
	cfg.InstagramComments = cfg.InstagramComments[:2]
	cfg.InstagramComments[0].Mentions = false
	out = h(t.Context(), "9", []metawebhook.Item{item})
	assertions.Equal(metawebhook.Unrouted, out[0].Class)
	cfg.InstagramComments[0].Mentions = true
	item.Value = []byte(`{"media_id":"bad"}`)
	out = h(t.Context(), "9", []metawebhook.Item{item})
	assertions.Equal(metawebhook.Invalid, out[0].Class)
}

func TestMentionWebhookAutomaticRetryProgressAndReplay(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	var reads, version atomic.Int32
	installInstagramGraphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, "/v26.0/200", r.URL.Path) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, tail, found := strings.Cut(r.URL.Query().Get("fields"), "mentioned_media.media_id(")
		if !assert.True(t, found) {
			return
		}
		id, _, found := strings.Cut(tail, ")")
		if !assert.True(t, found) {
			return
		}
		reads.Add(1)
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"mentioned_media": map[string]string{"id": id, "caption": fmt.Sprintf("revision %d", version.Load()), "media_type": "IMAGE", "timestamp": "2000-01-02T03:04:05Z"}}))
	})
	cfg := instagramWebhookConfig(t)
	cfg.InstagramComments[0].Mentions = true
	st := testutil.NewSQLiteTestStore(t)
	source, err := registerInstagramMentionsProfile(t.Context(), st, cfg.InstagramComments[0])
	requirements.NoError(err)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "spool")
	spool, err := metawebhook.OpenSpool(root, 1<<20, func() time.Time { return now })
	requirements.NoError(err)
	reg := metawebhook.NewRegistry()
	registerMetaWebhookHandlers(reg, cfg, st)
	changes := make([]map[string]any, 17)
	ids := make([]string, len(changes))
	for i := range changes {
		ids[i] = strconv.Itoa(1000 + i)
		changes[i] = map[string]any{"field": "mentions", "value": map[string]string{"media_id": ids[i]}}
	}
	body, err := json.Marshal(map[string]any{"object": "instagram", "entry": []any{map[string]any{"id": "200", "changes": changes}}})
	requirements.NoError(err)
	_, duplicate, err := spool.Accept("9", body)
	requirements.NoError(err)
	requirements.False(duplicate)
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg})
	summary, err := worker.DrainOnce(t.Context())
	requirements.NoError(err)
	assertions.Equal(16, summary.Applied)
	assertions.Equal(1, summary.Retried)
	now = now.Add(time.Minute)
	due, err := spool.Due(1)
	requirements.NoError(err)
	requirements.Len(due, 1)
	assertions.Zero(due[0].Attempts)
	// Completed items must survive reopening the spool and constructing a worker.
	spool, err = metawebhook.OpenSpool(root, 1<<20, func() time.Time { return now })
	requirements.NoError(err)
	worker = metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg})
	_, err = worker.DrainOnce(t.Context())
	requirements.NoError(err)
	stats, err := spool.Stats()
	requirements.NoError(err)
	assertions.Equal(1, stats["9"].Done)
	assertions.Equal(int32(17), reads.Load())
	rows, err := st.StoredMessagesContext(t.Context(), source.ID, "instagram-mention-json", ids)
	requirements.NoError(err)
	assertions.Len(rows, 17)
	version.Store(1)
	n, err := spool.Requeue(metawebhook.RequeueFilter{States: []metawebhook.State{metawebhook.StateDone}}, false)
	requirements.NoError(err)
	requirements.Equal(1, n)
	_, err = worker.DrainOnce(t.Context())
	requirements.NoError(err)
	now = now.Add(time.Minute)
	_, err = worker.DrainOnce(t.Context())
	requirements.NoError(err)
	stats, err = spool.Stats()
	requirements.NoError(err)
	assertions.Equal(1, stats["9"].Done)
	assertions.Equal(int32(34), reads.Load(), "explicit replay refreshes every mention")
	rows, err = st.StoredMessagesContext(t.Context(), source.ID, "instagram-mention-json", ids)
	requirements.NoError(err)
	requirements.Len(rows, 17)
	for _, id := range ids {
		text, err := st.GetMessageBodyText(rows[id].ID)
		requirements.NoError(err)
		assertions.Equal("revision 1", text)
	}
}
