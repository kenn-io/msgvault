//go:build sqlite_vec || pgvector

package cmd

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

const embeddingGemma2Alias = "embeddinggemma-2-914f7f89142e33e77833254d9c9b90c3cef7303b-text-fp32-768-v1"

func loadEmbeddingGemma2Config(t *testing.T) vector.Config {
	t.Helper()
	cfg, err := config.Load("testdata/embeddinggemma-2.toml", t.TempDir())
	require.NoError(t, err)
	return cfg.Vector
}

// This exercises the same configuration adapter used by message, person and
// attachment-document clients, without requiring a model or external service.
func TestEmbeddingGemma2TextHTTPConformance(t *testing.T) {
	check, must := assert.New(t), require.New(t)
	type request struct {
		Model      string          `json:"model"`
		Input      []string        `json:"input"`
		Dimensions json.RawMessage `json:"dimensions"`
	}
	calls := make(chan request, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Equal(http.MethodPost, r.Method)
		check.Equal("/v1/embeddings", r.URL.Path)
		var req request
		if !check.NoError(json.NewDecoder(r.Body).Decode(&req)) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		calls <- req
		// Return reversed rows to exercise the indexed OpenAI response contract.
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[len(req.Input)-1-i] = map[string]any{
				"index": i, "embedding": embeddingGemma2UnitVector(i),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		check.NoError(json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	t.Cleanup(server.Close)

	cfg := loadEmbeddingGemma2Config(t)
	check.Equal(embeddingGemma2Alias, cfg.Embeddings.Model)
	check.Equal(768, cfg.Embeddings.Dimension)
	check.Equal(4, cfg.Embeddings.BatchSize)
	check.Equal(120*time.Second, cfg.Embeddings.Timeout)
	check.Equal("title: none | text: ", cfg.Embeddings.DocumentPrefix)
	check.Equal("task: search result | query: ", cfg.Embeddings.QueryPrefix)
	cfg.Embeddings.Endpoint = server.URL + "/v1"
	client := embed.NewClient(openAIEmbedConfig(cfg, ""))

	documents, err := client.EmbedDocuments(t.Context(), []embed.DocumentInput{
		{Chunks: []string{"First synthetic chunk.", "Second synthetic chunk."}},
		{Chunks: []string{"Another document."}},
	})
	must.NoError(err)
	check.Equal([][][]float32{
		{embeddingGemma2UnitVector(0), embeddingGemma2UnitVector(1)},
		{embeddingGemma2UnitVector(2)},
	}, documents)

	query, err := client.EmbedQuery(t.Context(), "Find the synthetic document.")
	must.NoError(err)
	check.Equal(embeddingGemma2UnitVector(0), query)
	var squaredNorm float64
	for _, component := range query {
		squaredNorm += float64(component) * float64(component)
	}
	// 0.6 and 0.8 are rounded to float32 by the provider response decoder.
	check.InDelta(1, squaredNorm, 1e-7)

	legacy, err := client.Embed(t.Context(), []string{"Message worker chunk."})
	must.NoError(err)
	check.Equal([][]float32{embeddingGemma2UnitVector(0)}, legacy)

	for _, inputs := range [][]string{
		{"title: none | text: First synthetic chunk.", "title: none | text: Second synthetic chunk.", "title: none | text: Another document."},
		{"task: search result | query: Find the synthetic document."},
		{"title: none | text: Message worker chunk."},
	} {
		req := <-calls
		check.Equal(embeddingGemma2Alias, req.Model)
		check.Equal(inputs, req.Input)
		check.Nil(req.Dimensions, "dimension validates the response; it does not request truncation")
	}
}

func embeddingGemma2UnitVector(index int) []float32 {
	vec := make([]float32, 768)
	vec[index*2], vec[index*2+1] = 0.6, 0.8
	return vec
}

func TestEmbeddingGemma2PreservesProviderOutput(t *testing.T) {
	// Normalization belongs to the serving recipe. The existing client must
	// preserve finite nonunit output as well as already normalized output.
	provided := make([]float32, 768)
	provided[0], provided[1] = 3, 4
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": provided}},
		}))
	}))
	t.Cleanup(server.Close)
	cfg := loadEmbeddingGemma2Config(t)
	cfg.Embeddings.Endpoint = server.URL + "/v1"
	client := embed.NewClient(openAIEmbedConfig(cfg, ""))
	got, err := client.EmbedQuery(t.Context(), "Synthetic query.")
	require.NoError(t, err)
	assert.Equal(t, provided, got)
}

func TestEmbeddingGemma2RejectsInvalidProviderVectors(t *testing.T) {
	type invalidVector struct {
		name   string
		vector []any
	}
	var tests []invalidVector
	for _, width := range []int{128, 256, 512, 767, 769} {
		values := make([]any, width)
		for i := range values {
			values[i] = 1
		}
		tests = append(tests, invalidVector{fmt.Sprintf("width %d", width), values})
	}
	for _, invalid := range []struct {
		name  string
		first any
	}{
		{"zero norm", 0},
		{"null component", nil},
		{"nonfinite float32", math.MaxFloat64},
	} {
		values := make([]any, 768)
		for i := range values {
			values[i] = 0
		}
		values[0] = invalid.first
		tests = append(tests, invalidVector{invalid.name, values})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			check, must := assert.New(t), require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				check.NoError(json.NewEncoder(w).Encode(map[string]any{
					"data": []map[string]any{{"index": 0, "embedding": test.vector}},
				}))
			}))
			t.Cleanup(server.Close)
			cfg := loadEmbeddingGemma2Config(t)
			cfg.Embeddings.Endpoint = server.URL + "/v1"
			client := embed.NewClient(openAIEmbedConfig(cfg, ""))

			message, err := client.Embed(t.Context(), []string{"Synthetic message."})
			must.ErrorIs(err, vector.ErrInvalidProviderVector)
			check.Nil(message)
			documents, err := client.EmbedDocuments(t.Context(), []embed.DocumentInput{{Chunks: []string{"Synthetic document."}}})
			must.ErrorIs(err, vector.ErrInvalidProviderVector)
			check.Nil(documents)
			query, err := client.EmbedQuery(t.Context(), "Synthetic query.")
			must.ErrorIs(err, vector.ErrInvalidProviderVector)
			check.Nil(query)
		})
	}
}
