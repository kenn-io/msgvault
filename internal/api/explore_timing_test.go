//go:build fts5 && sqlite_vec

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func TestExploreServerTimings(t *testing.T) {
	t.Parallel()
	f := storetest.New(t)
	id := f.NewMessage().WithSubject("glacier").Create(t, f.Store)
	require.NoError(t, f.Store.UpsertFTS(id, "glacier", "", "", "", ""))
	vectorCfg := vector.Config{
		Enabled:    true,
		Embeddings: vector.EmbeddingsConfig{Model: "test-model", Dimension: 2, MaxInputChars: 1000},
	}
	backend := newRealCoverageBackend(t, vectorCfg, "active-matching")
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 2}, hybrid.Config{
		ExpectedFingerprint: vectorCfg.GenerationFingerprint(),
	})
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  f.Store, Engine: newExploreDuckDBFixture(t),
		HybridEngine: engine, Backend: backend, Logger: testLogger(),
	})
	for _, tc := range []struct {
		name, path, body string
		metrics          []string
	}{
		{"full text", "/api/v1/explore", `{"query":"glacier","search_mode":"full_text"}`, []string{"lexical", "candidates", "projection", "identities"}},
		{"semantic", "/api/v1/explore", `{"query":"glacier","search_mode":"semantic"}`, []string{"embedding", "retrieval", "candidates", "projection", "identities"}},
		{"grouping", "/api/v1/explore/groups", `{"grouping":["participant"]}`, []string{"grouping"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := postExploreJSON(t, srv, tc.path, tc.body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			timings := strings.Join(response.Result().Header.Values("Server-Timing"), ",")
			for _, metric := range tc.metrics {
				assert.Regexp(t, `(?:^|,)`+metric+`;dur=[0-9]+\.[0-9]+(?:;|,|$)`, timings)
			}
			if tc.name == "semantic" {
				assert.Contains(t, timings, `desc="exact"`)
			}
		})
	}
}
