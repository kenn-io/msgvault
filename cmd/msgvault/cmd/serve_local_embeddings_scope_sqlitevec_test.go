//go:build sqlite_vec

package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func newScheduledLocalEmbeddingsFixture(t *testing.T, account, extra string, syncAccount func(*store.Store) error) (*localActivationFixture, int64) {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) || !assert.Len(t, body.Input, 1) {
			http.Error(w, "invalid embedding request", http.StatusBadRequest)
			return
		}
		assert.Equal(t, "my-model", body.Model)
		_ = writeFakeEmbeddingResponse(w, len(body.Input), 4)
	}))
	t.Cleanup(endpoint.Close)
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig+fmt.Sprintf(`
[vector]
enabled = false
[vector.embeddings]
endpoint = %q
model = "my-model"
dimension = 4
document_prefix = "passage: "
query_prefix = "question: "
max_input_chars = 777
[chat]
server = %q
`, endpoint.URL+"/v1", endpoint.URL)+extra)
	f := &localActivationFixture{cfg: fixture.load(t)}
	require.NoError(t, os.MkdirAll(filepath.Dir(f.cfg.DatabaseDSN()), 0o700))
	var err error
	f.store, err = store.Open(f.cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.store.Close() })
	require.NoError(t, f.store.InitSchema())
	_, selectedID, err := testutil.CreateIndexedSourceMessage(f.store, account, "selected", "Selected orchard", "The orchard has apples.")
	require.NoError(t, err)
	f.state = &invocation{cfg: f.cfg, logger: testDiscardLogger()}
	f.ctx = withInvocation(t.Context(), f.state)
	f.handle = &vectorInitHandle{done: make(chan struct{})}
	close(f.handle.done)
	f.server = api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Logger: f.state.logger})
	f.scheduler = scheduler.New(func(context.Context, string) error {
		if syncAccount != nil {
			return syncAccount(f.store)
		}
		return nil
	})
	t.Cleanup(func() {
		<-f.scheduler.Stop().Done()
		_ = f.server.Shutdown(context.Background())
		f.handle.CloseFeatures(f.state.logger)
	})
	require.NoError(t, f.scheduler.AddAccount(account, "0 0 * * *"))
	f.runner = &localBuildTestRunner{t: t, home: f.cfg.HomeDir}
	f.controller = &localEmbeddingsController{state: f.state, store: f.store, dbPath: f.cfg.DatabaseDSN(), handle: f.handle, server: f.server, scheduler: f.scheduler, runner: f.runner, inspect: func(context.Context, embedlocal.Options) (embedlocal.Status, error) { return embedlocal.Status{}, nil }}
	return f, selectedID
}

func TestLocalEmbeddingsScheduledAfterSync(t *testing.T) {
	account := "archive@example.test"
	f, _ := newScheduledLocalEmbeddingsFixture(t, account, "", func(st *store.Store) error {
		_, _, err := testutil.CreateIndexedSourceMessage(st, account, "later", "Later orchard", "Another orchard message.")
		return err
	})
	require.NoError(t, f.controller.Enable(f.ctx, false, func(api.LocalEmbeddingsStatus) {}))
	require.NoError(t, f.scheduler.TriggerSync(account, true))
	require.Eventually(t, func() bool {
		results, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 10})
		return err == nil && len(results) == 2
	}, 10*time.Second, 50*time.Millisecond)
}

func TestLocalEmbeddingsScheduledRecoveryPreservesVisualSearch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	defaults := config.NewDefaultConfig().Vector.Multimodal
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: defaults.Model, Dimension: defaults.Dimension, Media: media.Policy{MaxBytes: 20 << 20, MaxPixels: 16_000_000, AllowStill: true, AllowVideo: true}})
	require.NoError(err)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText)
	require.NoError(err)
	manifestPath := filepath.Join(t.TempDir(), "visual-capabilities.json")
	require.NoError(writeVisualCapabilityManifest(manifestPath, manifest))
	t.Setenv(defaults.APIKeyEnv, "synthetic-visual-key")
	extra := fmt.Sprintf("\n[vector.embed.scope]\naccounts = [\"selected@example.test\"]\n[vector.multimodal]\nenabled = true\ncapabilities_file = %q\n", manifestPath)
	f, selectedID := newScheduledLocalEmbeddingsFixture(t, "selected@example.test", extra, nil)
	_, _, err = testutil.CreateIndexedSourceMessage(f.store, "excluded@example.test", "excluded", "Excluded orchard", "An excluded orchard.")
	require.NoError(err)
	f.handle.vf, err = setupVectorFeatures(f.ctx, f.store, f.cfg.DatabaseDSN(), false, vectorInitVisualOpener{})
	require.NoError(err)
	require.NotNil(f.handle.vf.Visual)
	visual := f.handle.vf.Visual
	f.runner.failBuild = true
	f.runner.beforeBuild = func() {
		_, err := f.handle.vf.Backend.CreateGeneration(f.ctx, f.handle.vf.Cfg.Embeddings.Model, f.handle.vf.Cfg.Embeddings.Dimension, f.handle.vf.Cfg.GenerationFingerprint())
		require.NoError(err)
	}
	require.Error(f.controller.Enable(f.ctx, false, func(api.LocalEmbeddingsStatus) {}))
	assert.Same(visual, f.handle.vf.Visual)
	require.NoError(f.scheduler.TriggerSync("selected@example.test", true))
	require.Eventually(func() bool {
		var generation int64
		err := f.store.DB().QueryRow("SELECT COALESCE(embed_gen, 0) FROM messages WHERE id = ?", selectedID).Scan(&generation)
		return err == nil && generation > 0
	}, 5*time.Second, 50*time.Millisecond, "scheduled recovery must embed the selected message")
	require.Eventually(func() bool {
		results, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 10})
		return err == nil && len(results) == 1
	}, 10*time.Second, 50*time.Millisecond)
}
