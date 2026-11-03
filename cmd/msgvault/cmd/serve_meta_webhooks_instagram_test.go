package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
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
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type instagramFixtureTransport struct {
	origin *url.URL
	base   http.RoundTripper
}

func (r instagramFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "graph.facebook.com" {
		return nil, errors.New("unexpected fixture host")
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host, clone.Host = r.origin.Scheme, r.origin.Host, r.origin.Host
	response, err := r.base.RoundTrip(clone)
	if err != nil {
		return nil, fmt.Errorf("send fixture request: %w", err)
	}
	return response, nil
}

func instagramWebhookConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := webhookTestConfig(t)
	cfg.InstagramComments = []config.InstagramCommentsSource{{Identifier: "example-instagram", AppID: "9", PageID: "100", InstagramAccountID: "200", TokenEnv: "EXAMPLE_INSTAGRAM_TOKEN", Enabled: true}}
	t.Setenv("EXAMPLE_INSTAGRAM_TOKEN", "synthetic-instagram-token")
	return cfg
}

func installInstagramGraphFixture(t *testing.T, extra ...http.HandlerFunc) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Empty(req.URL.Query().Get("access_token"))
		switch req.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`)
		case "/v26.0/300":
			_, _ = fmt.Fprint(w, `{"id":"300","caption":"Synthetic media","timestamp":"2000-01-02T03:04:05Z","owner":{"id":"200"}}`)
		case "/v26.0/400":
			_, _ = fmt.Fprint(w, `{"id":"400","text":"nodeorchard","timestamp":"2000-01-02T04:00:00Z","media":{"id":"300"},"from":{"id":"700","username":"example_customer"}}`)
		default:
			if len(extra) > 0 {
				extra[0](w, req)
				return
			}
			assert.Fail("unexpected fixture path", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	origin, err := url.Parse(server.URL)
	require.NoError(err)
	transport := &http.Transport{}
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: instagramFixtureTransport{origin: origin, base: transport}}
	t.Cleanup(func() {
		http.DefaultClient = previous
		transport.CloseIdleConnections()
	})
}

func TestInstagramWebhookSignedComments(t *testing.T) {
	for _, field := range []string{"comments", "live_comments"} {
		t.Run(field, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			installInstagramGraphFixture(t)
			cfg := instagramWebhookConfig(t)
			st := testutil.NewSQLiteTestStore(t)
			client, err := meta.NewClient("synthetic-instagram-token")
			require.NoError(err)
			src, err := meta.NewImporter(st, client).RegisterInstagram(t.Context(), "9", "100", "200")
			require.NoError(err)
			runtime, err := startMetaWebhooks(t.Context(), cfg, st, nil, nil)
			require.NoError(err)
			defer stopWebhook(t, runtime)
			body := fmt.Sprintf(`{"object":"instagram","entry":[{"id":"200","time":20,"changes":[{"field":%q,"value":{"id":"400","text":"payload must not be archived","media":{"id":"300","ad_id":"900"}}}]}]}`, field)
			postWebhook(t, runtime, body)
			engine := query.NewSQLiteEngine(st.DB())
			var found []query.MessageSummary
			require.Eventually(func() bool {
				found, err = engine.Search(t.Context(), &search.Query{TextTerms: []string{"nodeorchard"}}, 10, 0)
				return err == nil && len(found) == 1
			}, 30*time.Second, 10*time.Millisecond)
			require.NoError(err)
			assert.Equal("400", found[0].SourceMessageID)
			stored, err := st.StoredMessagesContext(t.Context(), src.ID, "instagram-comment-json", []string{"400"})
			require.NoError(err)
			assert.Equal(time.Date(2000, 1, 2, 4, 0, 0, 0, time.UTC), stored["400"].SentAt.Time.UTC())
			metadata, err := st.GetMessageMetadataContext(t.Context(), stored["400"].ID)
			require.NoError(err)
			assert.Contains(metadata.String, `"ad_id":"900"`)
			assert.NotContains(metadata.String, `"ad":true`)
		})
	}
}

func TestInstagramWebhookRouting(t *testing.T) {
	for _, tc := range []struct {
		name, app, account, class, reason string
		change                            func(*config.Config)
	}{
		{"wrong app", "8", "200", "unrouted", "profile_missing", nil},
		{"wrong account", "9", "201", "unrouted", "profile_missing", nil},
		{"disabled", "9", "200", "unrouted", "profile_missing", func(c *config.Config) { c.InstagramComments[0].Enabled = false }},
		{"missing token", "9", "200", "retry", "token_missing", func(c *config.Config) { c.InstagramComments[0].TokenEnv = "EXAMPLE_MISSING_INSTAGRAM_TOKEN" }},
		{"ambiguous Page", "9", "200", "unrouted", "profile_ambiguous", func(c *config.Config) {
			other := c.InstagramComments[0]
			other.PageID, other.Identifier = "101", "another-example"
			c.InstagramComments = append(c.InstagramComments, other)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := instagramWebhookConfig(t)
			t.Setenv("EXAMPLE_MISSING_INSTAGRAM_TOKEN", "")
			if tc.change != nil {
				tc.change(cfg)
			}
			factoryCalls := 0
			handler := instagramCommentHandler(cfg, nil, func(string) (meta.InstagramObjectReader, error) {
				factoryCalls++
				return nil, errors.New("unexpected factory")
			})
			out := handler(t.Context(), tc.app, []metawebhook.Item{{EntryID: tc.account, Object: "instagram", Field: "comments", Value: []byte(`{"id":"400","media":{"id":"300"}}`)}})
			require.Len(out, 1)
			assert.Equal(metawebhook.Class(tc.class), out[0].Class)
			assert.Equal(tc.reason, out[0].ErrorClass)
			assert.Zero(factoryCalls)
		})
	}
}

func TestInstagramWebhookInvalidValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := instagramWebhookConfig(t)
	handler := instagramCommentHandler(cfg, nil, func(string) (meta.InstagramObjectReader, error) {
		return nil, errors.New("unexpected factory")
	})
	out := handler(context.Background(), "9", []metawebhook.Item{{EntryID: "200", Value: []byte(`{"id":2}`)}})
	require.Len(out, 1)
	assert.Equal(metawebhook.Invalid, out[0].Class)
	assert.Equal("invalid_comment", out[0].ErrorClass)
}

func TestInstagramWebhookAutomaticRetryMakesProgressAndReplayRefreshes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	var reads, version atomic.Int64
	installInstagramGraphFixture(t, func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v26.0/200/media":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"300","caption":"Synthetic media","timestamp":"2000-01-02T03:04:05Z","owner":{"id":"200"}}]}`)
		case "/v26.0/300/comments":
			_, _ = fmt.Fprint(w, `{"data":[]}`)
		default:
			id := strings.TrimPrefix(request.URL.Path, "/v26.0/")
			n, err := strconv.Atoi(id)
			if !assert.NoError(err) || !assert.True(n >= 8000 && n <= 8050) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			reads.Add(1)
			assert.NoError(json.MarshalWrite(w, map[string]any{
				"id": id, "text": fmt.Sprintf("revision %d", version.Load()),
				"timestamp": "2000-01-02T04:00:00Z", "media": map[string]string{"id": "300"},
				"from": map[string]string{"id": "700", "username": "example_customer"},
			}))
		}
	})
	cfg := instagramWebhookConfig(t)
	st := testutil.NewSQLiteTestStore(t)
	client, err := meta.NewClient("synthetic-instagram-token")
	require.NoError(err)
	importer := meta.NewImporter(st, client)
	source, err := importer.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	_, err = importer.ImportInstagram(t.Context(), "9", "100", "200", meta.ImportOptions{MaxRequests: 3})
	require.NoError(err)
	roots, err := st.StoredMessagesContext(t.Context(), source.ID, "instagram-comment-json", []string{"300"})
	require.NoError(err)
	require.Contains(roots, "300")
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	spoolRoot := filepath.Join(t.TempDir(), "spool")
	spool, err := metawebhook.OpenSpool(spoolRoot, 1<<20, func() time.Time { return now })
	require.NoError(err)
	registry := metawebhook.NewRegistry()
	registerMetaWebhookHandlers(registry, cfg, st)
	changes := make([]map[string]any, 51)
	ids := make([]string, 51)
	for i := range changes {
		ids[i] = strconv.Itoa(8000 + i)
		changes[i] = map[string]any{"field": "comments", "value": map[string]any{"id": ids[i], "media": map[string]any{"id": "300"}}}
	}
	body, err := json.Marshal(map[string]any{"object": "instagram", "entry": []any{map[string]any{"id": "200", "time": 20, "changes": changes}}})
	require.NoError(err)
	_, duplicate, err := spool.Accept("9", body)
	require.NoError(err)
	require.False(duplicate)
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: registry})
	summary, err := worker.DrainOnce(t.Context())
	require.NoError(err)
	assert.Equal(50, summary.Applied)
	assert.Equal(1, summary.Retried)
	now = now.Add(31 * time.Second)
	due, err := spool.Due(1)
	require.NoError(err)
	require.Len(due, 1)
	assert.Zero(due[0].Attempts, "a request budget yields work rather than consuming a failure")
	// Reopen the spool and rebuild the worker to prove progress survives restart.
	spool, err = metawebhook.OpenSpool(spoolRoot, 1<<20, func() time.Time { return now })
	require.NoError(err)
	worker = metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: registry})
	_, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	stats, err := spool.Stats()
	require.NoError(err)
	assert.Equal(1, stats["9"].Done, "automatic retry must reach the final comment")
	assert.Equal(int64(51), reads.Load(), "completed comments must not consume the next pass budget")
	rows, err := st.StoredMessagesContext(t.Context(), source.ID, "instagram-comment-json", ids)
	require.NoError(err)
	assert.Len(rows, 51)
	version.Store(1)
	n, err := spool.Requeue(metawebhook.RequeueFilter{States: []metawebhook.State{metawebhook.StateDone}}, false)
	require.NoError(err)
	require.Equal(1, n)
	_, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	now = now.Add(31 * time.Second)
	_, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	stats, err = spool.Stats()
	require.NoError(err)
	assert.Equal(1, stats["9"].Done)
	assert.Equal(int64(102), reads.Load(), "explicit replay must refresh equal-time comments")
	rows, err = st.StoredMessagesContext(t.Context(), source.ID, "instagram-comment-json", ids)
	require.NoError(err)
	require.Len(rows, 51)
	for _, id := range ids {
		body, err := st.GetMessageBodyText(rows[id].ID)
		require.NoError(err)
		assert.Equal("revision 1", body)
	}
}

func TestInstagramWebhookPartialWriteRefreshesCache(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	installInstagramGraphFixture(t, func(w http.ResponseWriter, request *http.Request) {
		assert.Equal("/v26.0/401", request.URL.Path)
		_, _ = fmt.Fprint(w, `{"id":"401","text":"Example comment","timestamp":"invalid","media":{"id":"300"}}`)
	})
	cfg := instagramWebhookConfig(t)
	st := testutil.NewSQLiteTestStore(t)
	client, err := meta.NewClient("synthetic-instagram-token")
	require.NoError(err)
	src, err := meta.NewImporter(st, client).RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	spool, err := metawebhook.OpenSpool(t.TempDir(), 1<<20, time.Now)
	require.NoError(err)
	_, _, err = spool.Accept("9", []byte(`{"object":"instagram","entry":[{"id":"200","time":20,"changes":[{"field":"comments","value":{"id":"401","media":{"id":"300"}}}]}]}`))
	require.NoError(err)
	reg := metawebhook.NewRegistry()
	registerMetaWebhookHandlers(reg, cfg, st)
	refreshed := 0
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg, OnApplied: func() { refreshed++ }})
	summary, err := worker.DrainOnce(t.Context())
	require.NoError(err)
	assert.Zero(summary.Applied)
	rows, err := st.StoredMessagesContext(t.Context(), src.ID, "instagram-comment-json", []string{"300", "401"})
	require.NoError(err)
	assert.Contains(rows, "300")
	assert.NotContains(rows, "401")
	assert.Equal(1, refreshed)
}

func TestFacebookWebhookAutomaticRetryPreservesNewerEqualTimeEdit(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := webhookTestConfig(t)
	st, src := webhookArchive(t, true)
	now := time.Now()
	root := t.TempDir()
	spool, err := metawebhook.OpenSpool(root, 1<<20, func() time.Time { return now })
	require.NoError(err)
	reg := webhookRegistry(cfg, st)
	retryMention := true
	reg.Register("page", "mention", func(_ context.Context, _ string, items []metawebhook.Item) []metawebhook.ItemOutcome {
		out := make([]metawebhook.ItemOutcome, len(items))
		for i := range out {
			out[i].Class = metawebhook.Ignored
			if retryMention {
				out[i].Class = metawebhook.Retry
			}
		}
		return out
	})
	first := `{"object":"page","entry":[{"id":"100","time":20,"changes":[{"field":"feed","value":{"item":"comment","verb":"edited","post_id":"100_200","parent_id":"100_200","comment_id":"100_201","message":"first edit"}},{"field":"mention","value":{}}]}]}`
	id, _, err := spool.Accept("9", []byte(first))
	require.NoError(err)
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg})
	_, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	newer := strings.Replace(first, `{"field":"mention","value":{}}`, `{"field":"feed","value":{"item":"post","verb":"add"}}`, 1)
	newer = strings.Replace(newer, "first edit", "newest edit", 1)
	_, _, err = spool.Accept("9", []byte(newer))
	require.NoError(err)
	_, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	rows, err := st.StoredMessagesContext(t.Context(), src.ID, "facebook-comment-json", []string{"100_201"})
	require.NoError(err)
	require.Contains(rows, "100_201")
	body, err := st.GetMessageBodyText(rows["100_201"].ID)
	require.NoError(err)
	require.Equal("newest edit", body)
	now = now.Add(time.Minute)
	retryMention = false
	spool, err = metawebhook.OpenSpool(root, 1<<20, func() time.Time { return now })
	require.NoError(err)
	worker = metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg})
	_, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	body, err = st.GetMessageBodyText(rows["100_201"].ID)
	require.NoError(err)
	assert.Equal("newest edit", body, "automatic retry must not repeat an older applied edit")
	count, err := spool.Requeue(metawebhook.RequeueFilter{DeliveryID: id, States: []metawebhook.State{metawebhook.StateDone}}, false)
	require.NoError(err)
	require.Equal(1, count)
	_, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	body, err = st.GetMessageBodyText(rows["100_201"].ID)
	require.NoError(err)
	assert.Equal("first edit", body, "explicit replay still invokes completed equal-time edits")
}

func TestInstagramWebhookInterruptedBatchKeepsCommittedItems(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	var waiting atomic.Bool
	var firstReads, interruptedReads atomic.Int32
	installInstagramGraphFixture(t, func(w http.ResponseWriter, request *http.Request) {
		id := strings.TrimPrefix(request.URL.Path, "/v26.0/")
		switch id {
		case "401":
			firstReads.Add(1)
		case "402":
			if interruptedReads.Add(1) == 1 {
				waiting.Store(true)
				<-request.Context().Done()
				return
			}
		case "403":
		default:
			assert.Fail("unexpected fixture path", id)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		assert.NoError(json.MarshalWrite(w, map[string]any{
			"id": id, "text": "Example comment", "timestamp": "2000-01-02T04:00:00Z", "media": map[string]string{"id": "300"},
		}))
	})
	cfg := instagramWebhookConfig(t)
	st := testutil.NewSQLiteTestStore(t)
	client, err := meta.NewClient("synthetic-instagram-token")
	require.NoError(err)
	src, err := meta.NewImporter(st, client).RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	root := t.TempDir()
	spool, err := metawebhook.OpenSpool(root, 1<<20, time.Now)
	require.NoError(err)
	_, _, err = spool.Accept("9", []byte(`{"object":"instagram","entry":[{"id":"200","time":20,"changes":[{"field":"comments","value":{"id":"401","media":{"id":"300"}}},{"field":"comments","value":{"id":"402","media":{"id":"300"}}},{"field":"comments","value":{"id":"403","media":{"id":"300"}}}]}]}`))
	require.NoError(err)
	reg := metawebhook.NewRegistry()
	registerMetaWebhookHandlers(reg, cfg, st)
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg, ShouldYield: waiting.Load})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = worker.DrainOnce(ctx)
	require.NoError(err)
	due, err := spool.Due(1)
	require.NoError(err)
	require.Len(due, 1, "preemption leaves unfinished work immediately resumable")
	assert.Zero(due[0].Attempts)
	assert.Equal(map[int]metawebhook.Class{0: metawebhook.Applied}, due[0].CompletedItems)
	latest, err := st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	assert.Equal(int64(2), latest.MessagesAdded, "the media and first comment remain visible to cache refreshes after cancellation")
	waiting.Store(false)
	spool, err = metawebhook.OpenSpool(root, 1<<20, time.Now)
	require.NoError(err)
	worker = metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg})
	_, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	stats, err := spool.Stats()
	require.NoError(err)
	assert.Equal(1, stats["9"].Done)
	assert.Equal(int32(1), firstReads.Load(), "the committed first item survives restart without another read")
	assert.Equal(int32(2), interruptedReads.Load())
	rows, err := st.StoredMessagesContext(t.Context(), src.ID, "instagram-comment-json", []string{"401", "402", "403"})
	require.NoError(err)
	assert.Len(rows, 3)
}

func TestInstagramWebhookAnonymousEditRefreshesPublishedCache(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	var edited atomic.Bool
	installInstagramGraphFixture(t, func(w http.ResponseWriter, request *http.Request) {
		assert.Equal("/v26.0/401", request.URL.Path)
		text := "Original anonymous comment"
		if edited.Load() {
			text = "Updated anonymous comment"
		}
		assert.NoError(json.MarshalWrite(w, map[string]any{
			"id": "401", "text": text, "timestamp": "2000-01-02T04:00:00Z", "media": map[string]string{"id": "300"},
		}))
	})
	cfg := instagramWebhookConfig(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	analyticsDir := filepath.Join(t.TempDir(), "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	client, err := meta.NewClient("synthetic-instagram-token")
	require.NoError(err)
	src, err := meta.NewImporter(st, client).RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	spool, err := metawebhook.OpenSpool(t.TempDir(), 1<<20, time.Now)
	require.NoError(err)
	_, _, err = spool.Accept("9", []byte(`{"object":"instagram","entry":[{"id":"200","time":20,"changes":[{"field":"comments","value":{"id":"401","media":{"id":"300"}}}]}]}`))
	require.NoError(err)
	reg := metawebhook.NewRegistry()
	registerMetaWebhookHandlers(reg, cfg, st)
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg})
	_, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	_, err = buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err)
	for _, change := range []bool{false, true} {
		edited.Store(change)
		n, err := spool.Requeue(metawebhook.RequeueFilter{States: []metawebhook.State{metawebhook.StateDone}}, false)
		require.NoError(err)
		require.Equal(1, n)
		_, err = worker.DrainOnce(t.Context())
		require.NoError(err)
		latest, err := st.GetLatestSync(src.ID)
		require.NoError(err)
		if !change {
			assert.Zero(latest.MessagesUpdated)
			assert.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild, "equal-time unchanged replay must preserve the cache")
			continue
		}
		assert.Equal(int64(1), latest.MessagesUpdated)
		assert.True(cacheNeedsBuild(dbPath, analyticsDir).FullRebuild)
		_, err = buildCacheAuto(dbPath, analyticsDir)
		require.NoError(err)
	}
	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = duckDB.Close() })
	var snippet string
	require.NoError(duckDB.QueryRowContext(t.Context(), `SELECT snippet FROM read_parquet(?, hive_partitioning=true)
		WHERE source_message_id = '401'`, filepath.Join(analyticsDir, "messages", "**", "*.parquet")).Scan(&snippet))
	assert.Equal("Updated anonymous comment", snippet)
	assert.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild)
}
