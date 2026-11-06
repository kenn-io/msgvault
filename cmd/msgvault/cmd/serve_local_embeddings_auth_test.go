//go:build sqlite_vec || pgvector

package cmd

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func TestLocalEmbeddingsRecognizesAuthenticatedOllama(t *testing.T) {
	for _, credential := range []string{"stored", "environment"} {
		t.Run(credential, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newLocalActivationFixture(t, "nomic-embed-text", "")
			const key = "synthetic-ollama-key"
			var embeddings, rejectedCatalogs atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+key {
					if r.URL.Path == "/api/tags" {
						rejectedCatalogs.Add(1)
					}
					http.Error(w, "credential required", http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/api/tags":
					_, err := w.Write([]byte(`{"models":[{"name":"nomic-embed-text:latest"}]}`))
					assert.NoError(err)
				case "/v1/embeddings":
					var body struct {
						Input []string `json:"input"`
					}
					if !assert.NoError(json.UnmarshalRead(r.Body, &body)) {
						http.Error(w, "invalid request", http.StatusBadRequest)
						return
					}
					embeddings.Add(1)
					assert.NoError(writeFakeEmbeddingResponse(w, len(body.Input), 768))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(endpoint.Close)
			f.cfg.Vector.Embeddings.Endpoint = endpoint.URL + "/v1"
			f.cfg.Chat.Server = endpoint.URL
			if credential == "stored" {
				snapshot, err := providercredentials.Read(f.cfg.TokensDir())
				require.NoError(err)
				_, err = providercredentials.Put(f.cfg.TokensDir(), snapshot.ETag, providercredentials.VectorEmbeddingsID, f.cfg.Vector.Embeddings.Endpoint, key)
				require.NoError(err)
			} else {
				f.cfg.Vector.Embeddings.APIKeyEnv = "TEST_OLLAMA_EMBEDDING_KEY"
				t.Setenv("TEST_OLLAMA_EMBEDDING_KEY", key)
			}
			require.NoError(f.cfg.Save())
			f.start(t)
			messages, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 5})
			require.NoError(err, "the configured credential serves real indexing and search")
			require.Len(messages, 1)
			require.Positive(embeddings.Load())

			f.server.SetLocalEmbeddings(f.controller)
			status := f.localStatus(t)
			assert.Equal("ollama", status.Provider)
			assert.True(status.OllamaAvailable)
			require.NoError(f.controller.Enable(f.ctx, true, func(api.LocalEmbeddingsStatus) {}), "the independent built-in switch must remain available")
			assert.Zero(rejectedCatalogs.Load(), "provider recognition must use the configured credential")
		})
	}
}
