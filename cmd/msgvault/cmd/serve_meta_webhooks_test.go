package cmd

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
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

type webhookObjects struct{ includeComment bool }

func (webhookObjects) VerifyPage(context.Context, string) (meta.Page, error) {
	return meta.Page{ID: "100", Name: "Example Page", Category: "Community"}, nil
}
func (webhookObjects) Post(context.Context, string) (meta.Object, error) {
	return meta.Object{ID: "100_200", Message: "Example post", From: &meta.Actor{ID: "100", Name: "Example Page"}}, nil
}
func (webhookObjects) Comment(context.Context, string) (meta.Object, error) {
	return meta.Object{ID: "100_201", Message: "searchable webhook example", CreatedTime: "2026-10-01T12:00:00Z", Parent: &meta.Actor{ID: "100_200"}, From: &meta.Actor{ID: "300", Name: "Example Customer"}}, nil
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
	st := testutil.NewSQLiteTestStore(t)
	imp := meta.NewImporter(st, webhookObjects{includeComment: comments})
	src, err := imp.Register(t.Context(), "9", "100")
	require.NoError(t, err)
	_, err = imp.Import(t.Context(), "9", "100", meta.ImportOptions{})
	require.NoError(t, err)
	return st, src
}
func stopWebhook(t *testing.T, r *metaWebhookRuntime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, r.Shutdown(ctx))
}
func webhookBody() string {
	return `{"object":"page","entry":[{"id":"100","time":10,"changes":[{"field":"feed","value":{"item":"comment","verb":"add","post_id":"100_200","parent_id":"100_200","comment_id":"100_201","message":"searchable webhook example","created_time":"2026-10-01T12:00:00Z","from":{"id":"300","name":"Example Customer"}}}]}]}`
}
func postWebhook(t *testing.T, r *metaWebhookRuntime, body string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte("synthetic-app-secret"))
	_, _ = mac.Write([]byte(body))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+r.server.Addr+"/meta/webhook/9", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
func TestMetaWebhooksDisabledWithoutConfig(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	r, err := startMetaWebhooks(t.Context(), cfg, nil)
	require.NoError(t, err)
	assert.Nil(t, r.server)
	assert.Equal(t, "disabled", r.Snapshot().Listener)
	stopWebhook(t, r)
}
func TestMetaWebhooksDisabledReasonNamesEnv(t *testing.T) {
	cfg := webhookTestConfig(t)
	t.Setenv("EXAMPLE_META_VERIFY_TOKEN", "")
	r, err := startMetaWebhooks(t.Context(), cfg, nil)
	require.NoError(t, err)
	assert.Nil(t, r.server)
	assert.Contains(t, r.disabledReason, "EXAMPLE_META_VERIFY_TOKEN")
	assert.NotContains(t, r.disabledReason, "synthetic-app-secret")
	stopWebhook(t, r)
}
func TestMetaWebhooksDisabledWritesSnapshot(t *testing.T) {
	cfg := webhookTestConfig(t)
	t.Setenv("EXAMPLE_META_APP_SECRET", "")
	r, err := startMetaWebhooks(t.Context(), cfg, nil)
	require.NoError(t, err)
	defer stopWebhook(t, r)
	snap, err := metawebhook.ReadReceiverSnapshot(filepath.Join(cfg.Data.DataDir, "meta-webhooks"))
	require.NoError(t, err)
	assert.Contains(t, snap.DisabledReason, "EXAMPLE_META_APP_SECRET")
	assert.Equal(t, "disabled", snap.Listener)
}
func TestMetaWebhooksEndToEnd(t *testing.T) {
	cfg := webhookTestConfig(t)
	st, _ := webhookArchive(t, false)
	r, err := startMetaWebhooksWithRegistry(t.Context(), cfg, st, webhookRegistry(cfg, st))
	require.NoError(t, err)
	defer stopWebhook(t, r)
	postWebhook(t, r, webhookBody())
	engine := query.NewSQLiteEngine(st.DB())
	var got []query.MessageSummary
	assert.Eventually(t, func() bool {
		got, err = engine.Search(t.Context(), &search.Query{TextTerms: []string{"searchable"}}, 10, 0)
		return err == nil && len(got) == 1
	}, 8*time.Second, 10*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, got, 1)
	other, _ := webhookArchive(t, true)
	want, err := query.NewSQLiteEngine(other.DB()).Search(t.Context(), &search.Query{TextTerms: []string{"searchable"}}, 10, 0)
	require.NoError(t, err)
	require.Len(t, want, 1)
	assert.Equal(t, want[0].SourceMessageID, got[0].SourceMessageID)
	assert.Equal(t, want[0].ConversationID, got[0].ConversationID)
	assert.Equal(t, want[0].FromEmail, got[0].FromEmail)
	assert.Equal(t, want[0].Snippet, got[0].Snippet)
}
func TestMetaWebhooksCrashBetweenAcceptAndApply(t *testing.T) {
	cfg := webhookTestConfig(t)
	st, src := webhookArchive(t, false)
	spool, err := metawebhook.OpenSpool(filepath.Join(cfg.Data.DataDir, "meta-webhooks"), 1<<20, time.Now)
	require.NoError(t, err)
	_, _, err = spool.Accept("9", []byte(webhookBody()))
	require.NoError(t, err)
	// Drop the old spool without applying, as a crash after the 200 would do.
	r, err := startMetaWebhooksWithRegistry(t.Context(), cfg, st, webhookRegistry(cfg, st))
	require.NoError(t, err)
	defer stopWebhook(t, r)
	assert.Eventually(t, func() bool {
		ids, err := st.MessageExistsBatch(src.ID, []string{"100_201"})
		return err == nil && ids["100_201"] != 0
	}, 8*time.Second, 10*time.Millisecond)
}
func TestMetaWebhooksUnconfiguredPageHeld(t *testing.T) {
	cfg := webhookTestConfig(t)
	cfg.FacebookComments = nil
	st, _ := webhookArchive(t, false)
	r, err := startMetaWebhooksWithRegistry(t.Context(), cfg, st, webhookRegistry(cfg, st))
	require.NoError(t, err)
	defer stopWebhook(t, r)
	postWebhook(t, r, webhookBody())
	assert.Eventually(t, func() bool {
		stats, err := metawebhook.ReadStats(filepath.Join(cfg.Data.DataDir, "meta-webhooks"))
		return err == nil && stats["9"].Held == 1
	}, 8*time.Second, 10*time.Millisecond)
}
func TestMetaWebhooksShutdownDrains(t *testing.T) {
	cfg := webhookTestConfig(t)
	reg := metawebhook.NewRegistry()
	entered := make(chan struct{})
	reg.Register("page", "feed", func(ctx context.Context, _ string, _ []metawebhook.Item) []metawebhook.ItemOutcome {
		close(entered)
		<-ctx.Done()
		return []metawebhook.ItemOutcome{{Class: metawebhook.Retry}}
	})
	r, err := startMetaWebhooksWithRegistry(t.Context(), cfg, nil, reg)
	require.NoError(t, err)
	defer stopWebhook(t, r)
	postWebhook(t, r, webhookBody())
	select {
	case <-entered:
	case <-time.After(8 * time.Second):
		require.FailNow(t, "worker did not enter handler")
	}
	stopWebhook(t, r)
	states, err := filepath.Glob(filepath.Join(cfg.Data.DataDir, "meta-webhooks", "9", "pending", "*.state.json"))
	require.NoError(t, err)
	assert.Len(t, states, 1)
	_, err = os.Stat(filepath.Join(cfg.Data.DataDir, "meta-webhooks", "receiver.json"))
	assert.NoError(t, err)
}
