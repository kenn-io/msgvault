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
	"strings"
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
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func newScheduledLocalEmbeddingsFixture(t *testing.T, account, extra string, syncAccount func(*store.Store) error) (*localActivationFixture, int64) {
	t.Helper()
	f := &localActivationFixture{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"models": []map[string]string{{"name": "my-model"}}}))
			return
		}
		if raw := f.embeddingKey.Load(); raw != nil {
			key, ok := raw.(string)
			if !assert.True(t, ok, "test credential must be a string") {
				http.Error(w, "invalid test credential", http.StatusInternalServerError)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+key {
				http.Error(w, "embedding key was replaced", http.StatusUnauthorized)
				return
			}
		}
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
[chat]
server = %q
`, endpoint.URL+"/v1", endpoint.URL)+extra)
	f.cfg = fixture.load(t)
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

func TestLocalEmbeddingsScheduledRecoveryRespectsScope(t *testing.T) {
	require := require.New(t)
	extra := "\n[vector.embed.scope]\naccounts = [\"selected@example.test\"]\n"
	f, selectedID := newScheduledLocalEmbeddingsFixture(t, "selected@example.test", extra, nil)
	_, _, err := testutil.CreateIndexedSourceMessage(f.store, "excluded@example.test", "excluded", "Excluded orchard", "An excluded orchard.")
	require.NoError(err)
	f.runner.failBuild = true
	f.runner.beforeBuild = func() {
		_, err := f.handle.vf.Backend.CreateGeneration(f.ctx, f.handle.vf.Cfg.Embeddings.Model, f.handle.vf.Cfg.Embeddings.Dimension, f.handle.vf.Cfg.GenerationFingerprint())
		require.NoError(err)
	}
	require.Error(f.controller.Enable(f.ctx, false, func(api.LocalEmbeddingsStatus) {}))
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

func TestLocalEmbeddingsPublishesVisualRecoveryAfterFailedStartup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	defaults := config.NewDefaultConfig().Vector.Multimodal
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: defaults.Model, Dimension: defaults.Dimension, Media: media.Policy{MaxBytes: 20 << 20, MaxPixels: 16_000_000, AllowStill: true, AllowVideo: true}})
	require.NoError(err)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText)
	require.NoError(err)
	manifestPath := filepath.Join(t.TempDir(), "visual-capabilities.json")
	require.NoError(writeVisualCapabilityManifest(manifestPath, manifest))
	t.Setenv(defaults.APIKeyEnv, "synthetic-visual-key")
	extra := fmt.Sprintf("\n[vector.multimodal]\nenabled = true\ncapabilities_file = %q\n[vector.multimodal.schedule]\ncron = \"0 0 * * *\"\n", manifestPath)
	f := newLocalActivationFixture(t, "nomic-embed-text", extra)
	credentials, err := providercredentials.Read(f.cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(f.cfg.TokensDir(), credentials.ETag, providercredentials.VectorEmbeddingsID, "https://previous.example.test/v1", "synthetic-previous-key")
	require.NoError(err)
	f.handle = startVectorInit(f.ctx, f.store, f.cfg.DatabaseDSN(), nil, f.server, f.scheduler, vectorInitVisualOpener{})
	require.True(f.handle.WaitTimeout(5 * time.Second))
	f.controller.handle = f.handle
	f.controller.openers = []visual.StreamOpener{vectorInitVisualOpener{}}
	status, _ := f.server.VectorStatus()
	require.Equal(api.VectorStatusError, status)
	require.NoError(f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {}))
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{method: http.MethodGet, path: "/api/v1/multimodal/status", code: http.StatusOK},
		{method: http.MethodPost, path: "/api/v1/search/attachments/visual", body: `{}`, code: http.StatusBadRequest},
		{method: http.MethodPost, path: "/api/v1/multimodal/build", body: `{"consent":true}`, code: http.StatusOK},
	} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		request.Header.Set("Authorization", "Bearer owner")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		f.server.Router().ServeHTTP(response, request)
		assert.Equal(tc.code, response.Code, "%s: %s", tc.path, response.Body.String())
	}
	require.NoError(f.scheduler.TriggerJob("multimodal-attachments"))
}

func TestLocalEmbeddingsPausedOllamaUsesSavedCredentialWithVisualRuntime(t *testing.T) {
	require := require.New(t)
	defaults := config.NewDefaultConfig().Vector.Multimodal
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: defaults.Model, Dimension: defaults.Dimension, Media: media.Policy{MaxBytes: 20 << 20, MaxPixels: 16_000_000, AllowStill: true, AllowVideo: true}})
	require.NoError(err)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText)
	require.NoError(err)
	manifestPath := filepath.Join(t.TempDir(), "visual-capabilities.json")
	require.NoError(writeVisualCapabilityManifest(manifestPath, manifest))
	t.Setenv(defaults.APIKeyEnv, "synthetic-visual-key")
	extra := fmt.Sprintf("\n[vector.multimodal]\nenabled = true\ncapabilities_file = %q\n", manifestPath)
	f, _ := newScheduledLocalEmbeddingsFixture(t, "archive@example.test", extra, nil)
	credentials, err := providercredentials.Read(f.cfg.TokensDir())
	require.NoError(err)
	stored, err := providercredentials.Put(f.cfg.TokensDir(), credentials.ETag, providercredentials.VectorEmbeddingsID, f.cfg.Vector.Embeddings.Endpoint, "synthetic-startup-key")
	require.NoError(err)
	f.embeddingKey.Store("synthetic-startup-key")
	f.handle.vf, err = setupVectorFeatures(f.ctx, f.store, f.cfg.DatabaseDSN(), false, vectorInitVisualOpener{})
	require.NoError(err)
	require.NotNil(f.handle.vf.Visual)
	visualRuntime := f.handle.vf.Visual
	// Saving a replacement key leaves the running visual service in place.
	_, err = providercredentials.Put(f.cfg.TokensDir(), stored.ETag, providercredentials.VectorEmbeddingsID, f.cfg.Vector.Embeddings.Endpoint, "synthetic-replacement-key")
	require.NoError(err)
	f.embeddingKey.Store("synthetic-replacement-key")
	require.NoError(f.controller.Enable(f.ctx, false, func(api.LocalEmbeddingsStatus) {}))
	require.Same(visualRuntime, f.handle.vf.Visual)
	messages, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 5})
	require.NoError(err, "the published query runtime must use the same saved key as the successful builder")
	require.Len(messages, 1)
}
