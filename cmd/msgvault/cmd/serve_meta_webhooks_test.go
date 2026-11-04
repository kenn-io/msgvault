package cmd

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type webhookObjects struct {
	includeComment bool
	anonymous      bool
	body           string
}

func (webhookObjects) VerifyPage(context.Context, string) (meta.Page, error) {
	return meta.Page{ID: "100", Name: "Example Page", Category: "Community"}, nil
}
func (w webhookObjects) Post(context.Context, string) (meta.Object, error) {
	obj := meta.Object{ID: "100_200", Message: "Example post", From: &meta.Actor{ID: "100", Name: "Example Page"}}
	if w.anonymous {
		obj.From = nil
	}
	return obj, nil
}
func (w webhookObjects) Comment(context.Context, string) (meta.Object, error) {
	obj := meta.Object{ID: "100_201", Message: "searchable webhook example", CreatedTime: "2026-10-01T12:00:00Z", Parent: &meta.Actor{ID: "100_200"}, From: &meta.Actor{ID: "300", Name: "Example Customer"}}
	if w.anonymous {
		obj.From = nil
	}
	if w.body != "" {
		obj.Message = w.body
	}
	return obj, nil
}
func (w webhookObjects) Posts(ctx context.Context, _ string, edge meta.PostEdge, _ string) (meta.ObjectPage, error) {
	if edge != meta.Posts {
		return meta.ObjectPage{Data: []meta.Object{}}, nil
	}
	obj, err := w.Post(ctx, "100_200")
	return meta.ObjectPage{Data: []meta.Object{obj}}, err
}
func (w webhookObjects) Comments(ctx context.Context, parent, _ string) (meta.ObjectPage, error) {
	if !w.includeComment || parent != "100_200" {
		return meta.ObjectPage{Data: []meta.Object{}}, nil
	}
	obj, err := w.Comment(ctx, "100_201")
	return meta.ObjectPage{Data: []meta.Object{obj}}, err
}
func webhookTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = t.TempDir(), t.TempDir()
	cfg.MetaWebhooks.ListenAddr = "127.0.0.1:0" // Synthetic ephemeral listener, bypasses file validation.
	cfg.MetaWebhooks.Apps = []config.MetaWebhookApp{{AppID: "9", AppSecretEnv: "EXAMPLE_META_APP_SECRET", VerifyTokenEnv: "EXAMPLE_META_VERIFY_TOKEN"}}
	cfg.FacebookComments = []config.FacebookCommentsSource{facebookProfile()}
	t.Setenv("EXAMPLE_META_APP_SECRET", "synthetic-app-secret")
	t.Setenv("EXAMPLE_META_VERIFY_TOKEN", "synthetic-verify-token")
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	return cfg
}
func webhookRegistry(cfg *config.Config, st *store.Store) *metawebhook.Registry {
	reg := metawebhook.NewRegistry()
	reg.Register("page", "feed", feedHandler(cfg, st, func(string) (meta.ObjectReader, error) { return webhookObjects{}, nil }))
	return reg
}
func webhookArchive(t *testing.T, comments bool) (*store.Store, *store.Source) {
	t.Helper()
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	imp := meta.NewImporter(st, webhookObjects{includeComment: comments})
	src, err := imp.Register(t.Context(), "9", "100")
	require.NoError(err)
	_, err = imp.Import(t.Context(), "9", "100", meta.ImportOptions{})
	require.NoError(err)
	return st, src
}
func stopWebhook(t *testing.T, r *metaWebhookRuntime) {
	t.Helper()
	require := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(r.Shutdown(ctx))
}
func webhookBody() string {
	return `{"object":"page","entry":[{"id":"100","time":10,"changes":[{"field":"feed","value":{"item":"comment","verb":"add","post_id":"100_200","parent_id":"100_200","comment_id":"100_201","message":"searchable webhook example","created_time":"2026-10-01T12:00:00Z","from":{"id":"300","name":"Example Customer"}}}]}]}`
}
func postWebhook(t *testing.T, r *metaWebhookRuntime, body string) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	mac := hmac.New(sha256.New, []byte("synthetic-app-secret"))
	_, _ = mac.Write([]byte(body))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+r.server.Addr+"/meta/webhook/9", bytes.NewBufferString(body))
	require.NoError(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(req)
	require.NoError(err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(http.StatusOK, resp.StatusCode)
}
func TestMetaWebhooksDisabledWithoutConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	r, err := startMetaWebhooks(t.Context(), cfg, nil, nil, nil)
	require.NoError(err)
	assert.Nil(r.server)
	assert.Equal("disabled", r.Snapshot().Listener)
	stopWebhook(t, r)
}
func TestMetaWebhooksDisabledReasonNamesEnv(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := webhookTestConfig(t)
	t.Setenv("EXAMPLE_META_VERIFY_TOKEN", "")
	r, err := startMetaWebhooks(t.Context(), cfg, nil, nil, nil)
	require.NoError(err)
	assert.Nil(r.server)
	assert.Contains(r.disabledReason, "EXAMPLE_META_VERIFY_TOKEN")
	assert.NotContains(r.disabledReason, "synthetic-app-secret")
	stopWebhook(t, r)
}
func TestMetaWebhooksDisabledWritesSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := webhookTestConfig(t)
	t.Setenv("EXAMPLE_META_APP_SECRET", "")
	r, err := startMetaWebhooks(t.Context(), cfg, nil, nil, nil)
	require.NoError(err)
	defer stopWebhook(t, r)
	snap, err := metawebhook.ReadReceiverSnapshot(filepath.Join(cfg.Data.DataDir, "meta-webhooks"))
	require.NoError(err)
	assert.Contains(snap.DisabledReason, "EXAMPLE_META_APP_SECRET")
	assert.Equal("disabled", snap.Listener)
}
func TestMetaWebhooksEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := webhookTestConfig(t)
	st, _ := webhookArchive(t, false)
	r, err := startMetaWebhooksWithRegistry(t.Context(), cfg, st, webhookRegistry(cfg, st), nil, nil)
	require.NoError(err)
	defer stopWebhook(t, r)
	postWebhook(t, r, webhookBody())
	engine := query.NewSQLiteEngine(st.DB())
	var got []query.MessageSummary
	assert.Eventually(func() bool {
		got, err = engine.Search(t.Context(), &search.Query{TextTerms: []string{"searchable"}}, 10, 0)
		return err == nil && len(got) == 1
	}, 8*time.Second, 10*time.Millisecond)
	require.NoError(err)
	require.Len(got, 1)
	other, _ := webhookArchive(t, true)
	want, err := query.NewSQLiteEngine(other.DB()).Search(t.Context(), &search.Query{TextTerms: []string{"searchable"}}, 10, 0)
	require.NoError(err)
	require.Len(want, 1)
	assert.Equal(want[0].SourceMessageID, got[0].SourceMessageID)
	assert.Equal(want[0].ConversationID, got[0].ConversationID)
	assert.Equal(want[0].FromEmail, got[0].FromEmail)
	assert.Equal(want[0].Snippet, got[0].Snippet)
}

func TestMetaWebhookStateKeepsReceiptTimeForActionOrdering(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := webhookTestConfig(t)
	st, src := webhookArchive(t, true)
	receivedAt := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	now := receivedAt
	spool, err := metawebhook.OpenSpool(filepath.Join(cfg.Data.DataDir, "meta-webhooks"), 1<<20, func() time.Time { return now })
	require.NoError(err)
	body := `{"object":"page","entry":[{"id":"100","time":1793620800,"changes":[{"field":"feed","value":{"item":"comment","verb":"unhide","post_id":"100_200","parent_id":"100_200","comment_id":"100_201","message":"searchable webhook example"}}]}]}`
	_, _, err = spool.Accept("9", []byte(body))
	require.NoError(err)
	now = now.Add(time.Minute)
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: webhookRegistry(cfg, st)})
	summary, err := worker.DrainOnce(t.Context())
	require.NoError(err)
	assert.Equal(1, summary.Applied)
	ids, err := st.MessageExistsBatch(src.ID, []string{"100_201"})
	require.NoError(err)
	messageID := ids["100_201"]
	require.NotZero(messageID)
	saved, err := st.GetMessageMetadataContext(t.Context(), messageID)
	require.NoError(err)
	var ordering struct {
		StateObservedAt int64 `json:"state_observed_at"`
	}
	require.NoError(json.Unmarshal([]byte(saved.String), &ordering))
	assert.Equal(receivedAt.UnixNano(), ordering.StateObservedAt)
}
func TestMetaWebhooksCrashBetweenAcceptAndApply(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := webhookTestConfig(t)
	st, src := webhookArchive(t, false)
	spool, err := metawebhook.OpenSpool(filepath.Join(cfg.Data.DataDir, "meta-webhooks"), 1<<20, time.Now)
	require.NoError(err)
	_, _, err = spool.Accept("9", []byte(webhookBody()))
	require.NoError(err)
	// Drop the old spool without applying, as a crash after the 200 would do.
	r, err := startMetaWebhooksWithRegistry(t.Context(), cfg, st, webhookRegistry(cfg, st), nil, nil)
	require.NoError(err)
	defer stopWebhook(t, r)
	assert.Eventually(func() bool {
		ids, err := st.MessageExistsBatch(src.ID, []string{"100_201"})
		return err == nil && ids["100_201"] != 0
	}, 8*time.Second, 10*time.Millisecond)
}
func TestMetaWebhooksUnconfiguredPageHeld(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := webhookTestConfig(t)
	cfg.FacebookComments = nil
	st, _ := webhookArchive(t, false)
	r, err := startMetaWebhooksWithRegistry(t.Context(), cfg, st, webhookRegistry(cfg, st), nil, nil)
	require.NoError(err)
	defer stopWebhook(t, r)
	postWebhook(t, r, webhookBody())
	assert.Eventually(func() bool {
		stats, err := metawebhook.ReadStats(filepath.Join(cfg.Data.DataDir, "meta-webhooks"))
		return err == nil && stats["9"].Held == 1
	}, 8*time.Second, 10*time.Millisecond)
}
func TestMetaWebhooksShutdownDrains(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := webhookTestConfig(t)
	reg := metawebhook.NewRegistry()
	entered := make(chan struct{})
	reg.Register("page", "feed", func(ctx context.Context, _ string, _ []metawebhook.Item) []metawebhook.ItemOutcome {
		close(entered)
		<-ctx.Done()
		return []metawebhook.ItemOutcome{{Class: metawebhook.Retry}}
	})
	r, err := startMetaWebhooksWithRegistry(t.Context(), cfg, nil, reg, nil, nil)
	require.NoError(err)
	defer stopWebhook(t, r)
	postWebhook(t, r, webhookBody())
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		require.FailNow("worker did not enter handler")
	}
	stopWebhook(t, r)
	states, err := filepath.Glob(filepath.Join(cfg.Data.DataDir, "meta-webhooks", "9", "pending", "*.state.json"))
	require.NoError(err)
	assert.Len(states, 1)
	_, err = os.Stat(filepath.Join(cfg.Data.DataDir, "meta-webhooks", "receiver.json"))
	assert.NoError(err)
}

type webhookCancelGate struct {
	api.OperationGate

	cancel    context.CancelFunc
	attempted bool
}

func (g *webhookCancelGate) BeginWorkContext(ctx context.Context) (func(), bool) {
	g.attempted = true
	if g.cancel != nil {
		g.cancel()
	}
	return g.OperationGate.BeginWorkContext(ctx)
}

func TestMetaWebhooksApplyWaitsForOperationGate(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := webhookTestConfig(t)
	st, src := webhookArchive(t, false)
	gate := api.NewSerialOperationGate()
	release, ok := gate.BeginWorkContext(t.Context())
	require.True(ok)
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := &webhookCancelGate{OperationGate: gate, cancel: cancel}
	r, err := startMetaWebhooksWithRegistry(t.Context(), cfg, st, webhookRegistry(cfg, st), nil, observed)
	require.NoError(err)
	defer stopWebhook(t, r)
	r.cancel()
	<-r.workerDone
	// Acceptance remains available while a backup freezes archive writes.
	postWebhook(t, r, webhookBody())
	sum, err := r.worker.DrainOnce(ctx)
	require.NoError(err)
	assert.Zero(sum.Applied)
	assert.True(observed.attempted, "worker must request the archive gate before applying")
	observed.cancel = nil
	ids, err := st.MessageExistsBatch(src.ID, []string{"100_201"})
	require.NoError(err)
	assert.Empty(ids)
	release()
	sum, err = r.worker.DrainOnce(t.Context())
	require.NoError(err)
	assert.Equal(1, sum.Applied)
}

// This wrapper exercises the optional gate contract against a real archive
// gate. The broadcast integration owns the active-send lease implementation.
type webhookConsentGate struct {
	api.OperationGate

	consentCalls int
}

func (g *webhookConsentGate) BeginBroadcastConsentWorkContext(ctx context.Context) (func(), bool) {
	g.consentCalls++
	return g.BeginWorkContext(ctx)
}

func TestMetaWebhooksSelectsConsentGateForOptIns(t *testing.T) {
	for _, tc := range []struct {
		object, field string
		consentCalls  int
	}{
		{"page", "messaging_optins", 1},
		{"page", "feed", 0},
		{"instagram", "messages", 0},
	} {
		t.Run(tc.object+"/"+tc.field, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			cfg := webhookTestConfig(t)
			gate := &webhookConsentGate{OperationGate: api.NewSerialOperationGate()}
			reg := metawebhook.NewRegistry()
			reg.Register(tc.object, tc.field, func(_ context.Context, _ string, items []metawebhook.Item) []metawebhook.ItemOutcome {
				out := make([]metawebhook.ItemOutcome, len(items))
				for i := range out {
					out[i] = metawebhook.ItemOutcome{Class: metawebhook.Applied}
				}
				return out
			})
			receiver, err := startMetaWebhooksWithRegistry(t.Context(), cfg, nil, reg, nil, gate)
			require.NoError(err)
			defer stopWebhook(t, receiver)
			receiver.cancel()
			<-receiver.workerDone
			postWebhook(t, receiver, fmt.Sprintf(`{"object":%q,"entry":[{"id":"100","changes":[{"field":%q,"value":{}}]}]}`, tc.object, tc.field))
			summary, err := receiver.worker.DrainOnce(t.Context())
			require.NoError(err)
			assert.Equal(1, summary.Applied)
			assert.Equal(tc.consentCalls, gate.consentCalls, "only Page consent handlers may use the broadcast lease contract")
		})
	}
}

func TestMetaWebhooksPartialWriteRefreshesCache(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := webhookTestConfig(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := meta.NewImporter(st, webhookObjects{}).Register(t.Context(), "9", "100")
	require.NoError(err)
	spool, err := metawebhook.OpenSpool(t.TempDir(), 1<<20, time.Now)
	require.NoError(err)
	// The root commits before the comment's creation time is validated.
	body := strings.Replace(webhookBody(), "2026-10-01T12:00:00Z", "invalid", 1)
	_, _, err = spool.Accept("9", []byte(body))
	require.NoError(err)
	refreshed := 0
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: webhookRegistry(cfg, st), OnApplied: func() { refreshed++ }})
	sum, err := worker.DrainOnce(t.Context())
	require.NoError(err)
	assert.Zero(sum.Applied)
	ids, err := st.MessageExistsBatch(src.ID, []string{"100_200"})
	require.NoError(err)
	assert.Len(ids, 1)
	assert.Equal(1, refreshed)
}

// Each missing root consumes one real importer request. The fixture implements
// the provider read boundary so the worker, spool and Store remain unchanged.
type budgetWebhookObjects struct {
	webhookObjects

	reads  int
	denied map[string]bool
}

func (r *budgetWebhookObjects) Post(_ context.Context, id string) (meta.Object, error) {
	r.reads++
	if r.denied[id] {
		return meta.Object{}, &meta.APIError{Status: 403, Code: 200}
	}
	return meta.Object{ID: id, Message: "Example root"}, nil
}
func TestMetaWebhooksBudgetCompletesBeyondAttemptLimit(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := webhookTestConfig(t)
	st := testutil.NewSQLiteTestStore(t)
	_, err := meta.NewImporter(st, webhookObjects{}).Register(t.Context(), "9", "100")
	require.NoError(err)
	now := time.Now()
	spool, err := metawebhook.OpenSpool(t.TempDir(), 1<<20, func() time.Time { return now })
	require.NoError(err)
	var changes []string
	for i := range 601 {
		root, comment := fmt.Sprintf("100_%d", 1000+i), fmt.Sprintf("100_%d", 2000+i)
		changes = append(changes, fmt.Sprintf(`{"field":"feed","value":{"item":"comment","verb":"add","post_id":%q,"parent_id":%q,"comment_id":%q,"message":"Example comment"}}`, root, root, comment))
	}
	body := `{"object":"page","entry":[{"id":"100","time":10,"changes":[` + strings.Join(changes, ",") + `]}]}`
	_, _, err = spool.Accept("9", []byte(body))
	require.NoError(err)
	reader := &budgetWebhookObjects{}
	reg := metawebhook.NewRegistry()
	reg.Register("page", "feed", feedHandler(cfg, st, func(string) (meta.ObjectReader, error) { return reader, nil }))
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg})
	for pass := range 13 {
		_, err = worker.DrainOnce(t.Context())
		require.NoError(err)
		stats, err := spool.Stats()
		require.NoError(err)
		assert.Zero(stats["9"].Failed)
		if pass < 12 {
			assert.Equal(1, stats["9"].Pending)
		} else {
			assert.Equal(1, stats["9"].Done)
		}
		now = now.Add(time.Hour)
	}
	assert.Equal(601, reader.reads)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&count))
	assert.Equal(1202, count)
}

func TestMetaWebhooksBudgetReachesItemsAfterFailingAncestors(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := webhookTestConfig(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := meta.NewImporter(st, webhookObjects{}).Register(t.Context(), "9", "100")
	require.NoError(err)
	now := time.Now()
	dir := t.TempDir()
	spool, err := metawebhook.OpenSpool(dir, 1<<20, func() time.Time { return now })
	require.NoError(err)
	reader := &budgetWebhookObjects{denied: map[string]bool{}}
	var changes []string
	for i := range 651 {
		root, comment := fmt.Sprintf("100_%d", 1000+i), fmt.Sprintf("100_%d", 3000+i)
		if i < 650 {
			reader.denied[root] = true
		}
		changes = append(changes, fmt.Sprintf(`{"field":"feed","value":{"item":"comment","verb":"add","post_id":%q,"parent_id":%q,"comment_id":%q,"message":"Accessible tail"}}`, root, root, comment))
	}
	_, _, err = spool.Accept("9", []byte(`{"object":"page","entry":[{"id":"100","time":10,"changes":[`+strings.Join(changes, ",")+`]}]}`))
	require.NoError(err)
	reg := metawebhook.NewRegistry()
	reg.Register("page", "feed", feedHandler(cfg, st, func(string) (meta.ObjectReader, error) { return reader, nil }))
	for range 14 {
		// Restart the spool and worker; progress must survive process-local state.
		spool, err = metawebhook.OpenSpool(dir, 1<<20, func() time.Time { return now })
		require.NoError(err)
		_, err = metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg}).DrainOnce(t.Context())
		require.NoError(err)
		stats, err := spool.Stats()
		require.NoError(err)
		assert.Zero(stats["9"].Failed)
		now = now.Add(time.Hour)
	}
	ids, err := st.MessageExistsBatch(src.ID, []string{"100_3650"})
	require.NoError(err)
	require.NotZero(ids["100_3650"], "later accessible events must receive a turn before failures exhaust delivery attempts")
	body, err := st.GetMessageBodyText(ids["100_3650"])
	require.NoError(err)
	assert.Equal("Accessible tail", body)
}

// Fail the first provider node read so the first delivery has no completed item.
type retryOrderWebhookObjects struct {
	webhookObjects

	fail bool
}

func (r *retryOrderWebhookObjects) Post(ctx context.Context, id string) (meta.Object, error) {
	if r.fail {
		r.fail = false
		return meta.Object{}, errors.New("synthetic provider read unavailable")
	}
	return r.webhookObjects.Post(ctx, id)
}

func TestMetaWebhooksFailedItemRetryPreservesReceiptOrder(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := webhookTestConfig(t)
	st := testutil.NewSQLiteTestStore(t)
	reader := &retryOrderWebhookObjects{fail: true}
	src, err := meta.NewImporter(st, reader).Register(t.Context(), "9", "100")
	require.NoError(err)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(cfg.Data.DataDir, "meta-webhooks")
	spool, err := metawebhook.OpenSpool(root, 1<<20, func() time.Time { return now })
	require.NoError(err)
	registry := metawebhook.NewRegistry()
	registry.Register("page", "feed", feedHandler(cfg, st, func(string) (meta.ObjectReader, error) { return reader, nil }))
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: registry})
	oldBody := strings.Replace(webhookBody(), "searchable webhook example", "older failed edit", 1)
	newBody := strings.Replace(webhookBody(), "searchable webhook example", "later successful edit", 1)
	oldID, _, err := spool.Accept("9", []byte(oldBody))
	require.NoError(err)
	summary, err := worker.DrainOnce(t.Context())
	require.NoError(err)
	require.Equal(1, summary.Retried)
	// Both receipts intentionally use the same local and provider timestamps.
	_, _, err = spool.Accept("9", []byte(newBody))
	require.NoError(err)
	summary, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	require.Equal(1, summary.Applied)
	// Simulate a crash after the old delivery's body move but before its
	// receipt sidecar moves. Recovery must preserve the original ordering.
	require.NoError(os.Rename(filepath.Join(root, "9", "pending", oldID+".json"), filepath.Join(root, "9", "held", oldID+".json")))
	spool, err = metawebhook.OpenSpool(root, 1<<20, func() time.Time { return now })
	require.NoError(err)
	worker = metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: registry})
	now = now.Add(time.Minute)
	_, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	rows, err := st.StoredMessagesContext(t.Context(), src.ID, "facebook-comment-json", []string{"100_201"})
	require.NoError(err)
	require.Contains(rows, "100_201")
	body, err := st.GetMessageBodyText(rows["100_201"].ID)
	require.NoError(err)
	assert.Equal("later successful edit", body, "an automatic retry must retain its original receipt order")
	// Explicit replay is a new application request and still reapplies an equal-time body.
	n, err := spool.Requeue(metawebhook.RequeueFilter{AppID: "9", DeliveryID: oldID, States: []metawebhook.State{metawebhook.StateDone}}, false)
	require.NoError(err)
	require.Equal(1, n)
	_, err = worker.DrainOnce(t.Context())
	require.NoError(err)
	body, err = st.GetMessageBodyText(rows["100_201"].ID)
	require.NoError(err)
	assert.Equal("older failed edit", body)
}

func TestMetaWebhooksHeartbeatWhileDisabledOrDraining(t *testing.T) {
	originalInterval := metaWebhookHeartbeatInterval
	metaWebhookHeartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() { metaWebhookHeartbeatInterval = originalInterval })
	for _, disabled := range []bool{true, false} {
		t.Run(strconv.FormatBool(disabled), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			cfg := webhookTestConfig(t)
			if disabled {
				t.Setenv("EXAMPLE_META_APP_SECRET", "")
			}
			entered := make(chan struct{})
			reg := metawebhook.NewRegistry()
			reg.Register("page", "feed", func(ctx context.Context, _ string, _ []metawebhook.Item) []metawebhook.ItemOutcome {
				close(entered)
				<-ctx.Done()
				return []metawebhook.ItemOutcome{{Class: metawebhook.Retry}}
			})
			r, err := startMetaWebhooksWithRegistry(t.Context(), cfg, nil, reg, nil, nil)
			require.NoError(err)
			t.Cleanup(func() { stopWebhook(t, r) })
			if !disabled {
				postWebhook(t, r, webhookBody())
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					require.FailNow("handler did not start")
				}
			}
			initial, err := metawebhook.ReadReceiverSnapshot(r.root)
			require.NoError(err)
			require.Eventually(func() bool {
				snap, err := metawebhook.ReadReceiverSnapshot(r.root)
				return err == nil && snap.WrittenAt.After(initial.WrittenAt)
			}, 5*time.Second, time.Millisecond)
			report := metaWebhooksStatus(t.Context(), cfg, nil)
			if disabled {
				assert.Equal("disabled", report.Listener)
				assert.Contains(report.DisabledReason, "EXAMPLE_META_APP_SECRET")
			} else {
				assert.Equal("listening", report.Listener)
			}
		})
	}
}

func TestMetaWebhooksYieldToWaitingWriterWithoutFailure(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := webhookTestConfig(t)
	gate := api.NewSerialOperationGate()
	reg := metawebhook.NewRegistry()
	entered := make(chan struct{})
	var calls atomic.Int32
	reg.Register("page", "feed", func(ctx context.Context, _ string, _ []metawebhook.Item) []metawebhook.ItemOutcome {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done() // Models a Graph read that honors request cancellation.
			return []metawebhook.ItemOutcome{{Class: metawebhook.Applied}, {Class: metawebhook.Retry, ErrorClass: string(meta.ClassCancelled), CountAttempt: true}}
		}
		return []metawebhook.ItemOutcome{{Class: metawebhook.Applied}, {Class: metawebhook.Applied}}
	})
	r, err := startMetaWebhooksWithRegistry(t.Context(), cfg, nil, reg, nil, gate)
	require.NoError(err)
	t.Cleanup(func() { stopWebhook(t, r) })
	postWebhook(t, r, `{"object":"page","entry":[{"id":"100","changes":[{"field":"feed","value":{}},{"field":"feed","value":{}}]}]}`)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		require.FailNow("handler did not start")
	}
	requestCtx, cancelRequest := context.WithCancel(t.Context())
	defer cancelRequest()
	admitted := make(chan bool, 1)
	release := make(chan struct{})
	releaseWriter := sync.OnceFunc(func() { close(release) })
	defer releaseWriter()
	go func() {
		done, ok := gate.BeginRequestWorkContext(requestCtx, "synthetic mutation")
		admitted <- ok
		if ok {
			<-release
			done()
		}
	}()
	select {
	case ok := <-admitted:
		require.True(ok)
	case <-time.After(10 * time.Second):
		require.FailNow("webhook handler did not yield to the waiting writer")
	}
	var due []metawebhook.DeliveryMeta
	require.Eventually(func() bool {
		var readErr error
		due, readErr = r.spool.Due(-1)
		return readErr == nil && len(due) == 1 && due[0].Outcomes["page/feed/retry"] == 1
	}, 5*time.Second, time.Millisecond)
	assert.Zero(due[0].Attempts, "yielding must not consume a failure attempt")
	assert.True(due[0].NextAttemptAt.IsZero(), "resume after the waiting writer without failure backoff")
	releaseWriter()
	require.Eventually(func() bool {
		stats, err := r.spool.Stats()
		return err == nil && stats["9"].Done == 1
	}, 10*time.Second, time.Millisecond)
	assert.Equal(int32(2), calls.Load(), "unfinished delivery resumes after the interactive writer")
}

func TestMetaWebhooksCacheRefreshTracksActualChanges(t *testing.T) {
	for _, path := range []string{"polling", "webhook"} {
		for _, anonymous := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/anonymous=%t", path, anonymous), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				dbPath := filepath.Join(t.TempDir(), "archive.db")
				analyticsDir := filepath.Join(t.TempDir(), "analytics")
				st := testutil.NewSQLiteTestStoreAt(t, dbPath)
				objects := webhookObjects{includeComment: true, anonymous: anonymous}
				imp := meta.NewImporter(st, objects)
				src, err := imp.Register(t.Context(), "9", "100")
				require.NoError(err)
				_, err = imp.Import(t.Context(), "9", "100", meta.ImportOptions{})
				require.NoError(err)
				_, err = buildCacheAuto(dbPath, analyticsDir)
				require.NoError(err)

				_, err = imp.Import(t.Context(), "9", "100", meta.ImportOptions{})
				require.NoError(err)
				assert.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild, "unchanged polling must preserve the published cache")
				_, err = buildCacheAuto(dbPath, analyticsDir)
				require.NoError(err)

				objects.body = "updated anonymous-safe example"
				if path == "polling" {
					_, err = meta.NewImporter(st, objects).Import(t.Context(), "9", "100", meta.ImportOptions{})
				} else {
					obj, readErr := objects.Comment(t.Context(), "100_201")
					require.NoError(readErr)
					_, err = meta.NewEventImporter(st, objects).ApplyFeedEvents(t.Context(), "9", "100", []meta.FeedEvent{{
						Time: 20, Item: "comment", Verb: "edit", PostID: "100_200", CommentID: obj.ID,
						ParentID: "100_200", Message: obj.Message, CreatedTime: obj.CreatedTime, From: obj.From,
					}}, meta.EventOptions{})
				}
				require.NoError(err)
				latest, err := st.GetLatestSync(src.ID)
				require.NoError(err)
				assert.Equal(int64(1), latest.MessagesUpdated)
				assert.True(cacheNeedsBuild(dbPath, analyticsDir).FullRebuild, "an existing comment edit must refresh cached snippets")
				_, err = buildCacheAuto(dbPath, analyticsDir)
				require.NoError(err)
				duckDB, err := sql.Open("duckdb", "")
				require.NoError(err)
				t.Cleanup(func() { _ = duckDB.Close() })
				var snippet string
				require.NoError(duckDB.QueryRowContext(t.Context(), `SELECT snippet FROM read_parquet(?, hive_partitioning=true)
					WHERE source_message_id = '100_201'`, filepath.Join(analyticsDir, "messages", "**", "*.parquet")).Scan(&snippet))
				assert.Equal(objects.body, snippet)
				assert.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild, "the refreshed cache must converge")
			})
		}
	}
}
