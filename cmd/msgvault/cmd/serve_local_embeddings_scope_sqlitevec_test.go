//go:build sqlite_vec

package cmd

import (
	"context"
	"fmt"
	"io"
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

func TestLocalEmbeddingsScheduledRecoveryKeepsResolvedScopeWithVisualSearch(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0,0,0]}]}`)
	}))
	t.Cleanup(endpoint.Close)
	defaults := config.NewDefaultConfig().Vector.Multimodal
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: defaults.Model, Dimension: defaults.Dimension,
		Media: media.Policy{MaxBytes: 20 << 20, MaxPixels: 16_000_000, AllowStill: true, AllowVideo: true}})
	require.NoError(t, err)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText)
	require.NoError(t, err)
	manifestPath := filepath.Join(t.TempDir(), "visual-capabilities.json")
	require.NoError(t, writeVisualCapabilityManifest(manifestPath, manifest))
	t.Setenv(defaults.APIKeyEnv, "synthetic-visual-key")
	fixture := newSetupProvidersFixture(t, setupProvidersMinimalConfig+fmt.Sprintf(`
[vector]
enabled = false
[vector.embeddings]
endpoint = %q
model = "custom-local-model"
dimension = 4
[vector.embed.scope]
accounts = ["selected@example.test"]
[vector.multimodal]
enabled = true
capabilities_file = %q
[chat]
server = %q
`, endpoint.URL+"/v1", manifestPath, endpoint.URL))
	cfg := fixture.load(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.DatabaseDSN()), 0o700))
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	_, selectedID, err := testutil.CreateIndexedSourceMessage(st, "selected@example.test", "selected", "Selected orchard", "The orchard has apples.")
	require.NoError(t, err)
	_, excludedID, err := testutil.CreateIndexedSourceMessage(st, "excluded@example.test", "excluded", "Excluded orchard", "An excluded orchard.")
	require.NoError(t, err)
	state := &invocation{cfg: cfg, logger: testDiscardLogger()}
	ctx := withInvocation(t.Context(), state)
	vf, err := setupVectorFeatures(ctx, st, cfg.DatabaseDSN(), false, vectorInitVisualOpener{})
	require.NoError(t, err)
	require.NotNil(t, vf.Visual)
	visual := vf.Visual
	handle := &vectorInitHandle{done: make(chan struct{}), vf: vf}
	close(handle.done)
	t.Cleanup(func() { handle.CloseFeatures(state.logger) })
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: state.logger})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	sched := scheduler.New(func(context.Context, string) error { return nil })
	t.Cleanup(func() { <-sched.Stop().Done() })
	require.NoError(t, sched.AddAccount("selected@example.test", "0 0 * * *"))
	runner := &localBuildTestRunner{t: t, home: cfg.HomeDir, failBuild: true, beforeBuild: func() {
		_, err := handle.vf.Backend.CreateGeneration(ctx, handle.vf.Cfg.Embeddings.Model, handle.vf.Cfg.Embeddings.Dimension, handle.vf.Cfg.GenerationFingerprint())
		require.NoError(t, err)
	}}
	controller := &localEmbeddingsController{state: state, store: st, dbPath: cfg.DatabaseDSN(), handle: handle, server: server,
		scheduler: sched, runner: runner,
		inspect: func(context.Context, embedlocal.Options) (embedlocal.Status, error) { return embedlocal.Status{}, nil }}
	require.Error(t, controller.Enable(ctx, false, func(api.LocalEmbeddingsStatus) {}))
	assert.Same(t, visual, handle.vf.Visual)
	require.NoError(t, sched.TriggerSync("selected@example.test", true))
	require.Eventually(t, func() bool {
		var generation int64
		err := st.DB().QueryRow("SELECT COALESCE(embed_gen, 0) FROM messages WHERE id = ?", selectedID).Scan(&generation)
		return err == nil && generation > 0
	}, 5*time.Second, 50*time.Millisecond, "scheduled recovery must embed the selected message")
	var excludedGeneration int64
	require.NoError(t, st.DB().QueryRow("SELECT COALESCE(embed_gen, 0) FROM messages WHERE id = ?", excludedID).Scan(&excludedGeneration))
	assert.Zero(t, excludedGeneration, "the account scope must leave excluded text local")
	require.Eventually(t, func() bool {
		results, _, err := handle.vf.HybridEngine.Search(ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 10})
		return err == nil && len(results) == 1
	}, 10*time.Second, 50*time.Millisecond, "scheduled recovery should activate the selected account's index")
}
