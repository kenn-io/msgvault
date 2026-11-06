//go:build sqlite_vec

package cmd

import (
	"context"
	"encoding/json/v2"
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

func TestLocalEmbeddingsScheduledRecovery(t *testing.T) {
	for _, failedBuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("scoped visual failure=%t", failedBuild), func(t *testing.T) {
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
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0,0,0]}]}`)
			}))
			t.Cleanup(endpoint.Close)
			account := "archive@example.test"
			extra := ""
			if failedBuild {
				account = "selected@example.test"
				defaults := config.NewDefaultConfig().Vector.Multimodal
				policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: defaults.Model, Dimension: defaults.Dimension, Media: media.Policy{MaxBytes: 20 << 20, MaxPixels: 16_000_000, AllowStill: true, AllowVideo: true}})
				require.NoError(t, err)
				manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText)
				require.NoError(t, err)
				manifestPath := filepath.Join(t.TempDir(), "visual-capabilities.json")
				require.NoError(t, writeVisualCapabilityManifest(manifestPath, manifest))
				t.Setenv(defaults.APIKeyEnv, "synthetic-visual-key")
				extra = fmt.Sprintf("\n[vector.embed.scope]\naccounts = [\"selected@example.test\"]\n[vector.multimodal]\nenabled = true\ncapabilities_file = %q\n", manifestPath)
			}
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
			cfg := fixture.load(t)
			require.NoError(t, os.MkdirAll(filepath.Dir(cfg.DatabaseDSN()), 0o700))
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(t, err)
			t.Cleanup(func() { _ = st.Close() })
			require.NoError(t, st.InitSchema())
			_, selectedID, err := testutil.CreateIndexedSourceMessage(st, account, "selected", "Selected orchard", "The orchard has apples.")
			require.NoError(t, err)
			var excludedID int64
			if failedBuild {
				_, excludedID, err = testutil.CreateIndexedSourceMessage(st, "excluded@example.test", "excluded", "Excluded orchard", "An excluded orchard.")
				require.NoError(t, err)
			}
			state := &invocation{cfg: cfg, logger: testDiscardLogger()}
			ctx := withInvocation(t.Context(), state)
			handle := &vectorInitHandle{done: make(chan struct{})}
			close(handle.done)
			if failedBuild {
				handle.vf, err = setupVectorFeatures(ctx, st, cfg.DatabaseDSN(), false, vectorInitVisualOpener{})
				require.NoError(t, err)
				require.NotNil(t, handle.vf.Visual)
			}
			t.Cleanup(func() { handle.CloseFeatures(state.logger) })
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: state.logger})
			t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
			sched := scheduler.New(func(context.Context, string) error {
				if failedBuild {
					return nil
				}
				_, _, err := testutil.CreateIndexedSourceMessage(st, account, "later", "Later orchard", "Another orchard message.")
				return err
			})
			t.Cleanup(func() { <-sched.Stop().Done() })
			require.NoError(t, sched.AddAccount(account, "0 0 * * *"))
			runner := &localBuildTestRunner{t: t, home: cfg.HomeDir, failBuild: failedBuild}
			controller := &localEmbeddingsController{state: state, store: st, dbPath: cfg.DatabaseDSN(), handle: handle, server: server, scheduler: sched, runner: runner, inspect: func(context.Context, embedlocal.Options) (embedlocal.Status, error) { return embedlocal.Status{}, nil }}
			if failedBuild {
				visual := handle.vf.Visual
				runner.beforeBuild = func() {
					_, err := handle.vf.Backend.CreateGeneration(ctx, handle.vf.Cfg.Embeddings.Model, handle.vf.Cfg.Embeddings.Dimension, handle.vf.Cfg.GenerationFingerprint())
					require.NoError(t, err)
				}
				require.ErrorContains(t, controller.Enable(ctx, false, func(api.LocalEmbeddingsStatus) {}), "embedding scope matched 0 live messages")
				assert.Same(t, visual, handle.vf.Visual)
			} else {
				require.NoError(t, controller.Enable(ctx, false, func(api.LocalEmbeddingsStatus) {}))
			}
			require.NoError(t, sched.TriggerSync(account, true))
			if failedBuild {
				require.Eventually(t, func() bool {
					var generation int64
					err := st.DB().QueryRow("SELECT COALESCE(embed_gen, 0) FROM messages WHERE id = ?", selectedID).Scan(&generation)
					return err == nil && generation > 0
				}, 5*time.Second, 50*time.Millisecond, "scheduled recovery must embed the selected message")
				var excludedGeneration int64
				require.NoError(t, st.DB().QueryRow("SELECT COALESCE(embed_gen, 0) FROM messages WHERE id = ?", excludedID).Scan(&excludedGeneration))
				assert.Zero(t, excludedGeneration)
			}
			want := 2
			if failedBuild {
				want = 1
			}
			require.Eventually(t, func() bool {
				results, _, err := handle.vf.HybridEngine.Search(ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 10})
				return err == nil && len(results) == want
			}, 10*time.Second, 50*time.Millisecond)
		})
	}
}
