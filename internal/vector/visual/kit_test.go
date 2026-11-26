package visual

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type kitSearchBackend struct{ *fakeVisualBackend }

func (b *kitSearchBackend) Search(_ context.Context, request SearchRequest) ([]Hit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var hits []Hit
	for token, vector := range b.vectors {
		score := float64(vector[0]*request.Vector[0] + vector[1]*request.Vector[1])
		hits = append(hits, Hit{Token: token, Score: score, Rank: 1})
	}
	return hits, nil
}

func TestKitPhotoBuildAndTextSearch(t *testing.T) {
	requests := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- body
		vector := make([]float32, 1024)
		vector[0], vector[1] = 3, 4
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vector}}})
	}))
	t.Cleanup(server.Close)
	provider, err := NewKitProvider("kit", server.URL+"/v1", "gemini-embedding-2", 1024, "", server.Client(), "generation")
	require.NoError(t, err)
	f := storetest.New(t)
	generation, err := f.Store.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{Fingerprint: "generation", Model: "gemini-embedding-2", Dimension: 1024})
	require.NoError(t, err)
	messageID := testVisualCandidate(t, f, "kit-photo", strings.Repeat("a1", 32))
	photo := encodedPNG(t, 2, 2)
	reconciler := testReconciler(t, f, generation.ID, memoryOpener{data: photo}, "visual-test/kit-photo")
	reconciler.config.ContextPolicy.ExcludeContext = true
	work, err := reconciler.FullReconcile(t.Context())
	require.NoError(t, err)
	require.Len(t, work.Work, 1)
	backend := &kitSearchBackend{newFakeVisualBackend()}
	worker, err := NewWorker(f.Store, provider, backend, WorkerConfig{Dimension: 1024, ProviderTimeout: time.Second, LeaseDuration: 2 * time.Second})
	require.NoError(t, err)
	result, err := worker.Run(t.Context(), work.Work)
	require.NoError(t, err)
	require.Equal(t, int64(1), result.Published)
	_, err = reconciler.FullReconcile(t.Context())
	require.NoError(t, err)
	require.NoError(t, f.Store.ConsentVisualGeneration(t.Context(), generation.ID, "generation"))
	_, err = reconciler.Activate(t.Context())
	require.NoError(t, err)
	search, err := NewSearchService(f.Store, provider, backend, false)
	require.NoError(t, err)
	response, err := search.Search(t.Context(), SearchQuery{Text: "beach", Limit: 5})
	require.NoError(t, err)
	require.Len(t, response.Results, 1)
	assert.Equal(t, messageID, response.Results[0].MessageID)
	documentRequest, queryRequest := <-requests, <-requests
	assert.Equal(t, "gemini-embedding-2", documentRequest["model"])
	assert.Equal(t, []any{map[string]any{"role": "document", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(photo)}}, documentRequest["inputs"])
	assert.Equal(t, "gemini-embedding-2", queryRequest["model"])
	assert.Equal(t, []any{map[string]any{"role": "query", "text": "task: search result | query: beach"}}, queryRequest["inputs"])
}

func TestKitProviderInputs(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embed", r.URL.Path)
		var body map[string]any
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			return
		}
		requests = append(requests, body)
		_, err := w.Write([]byte(`{"data":[{"index":0,"embedding":[3,4]}]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	p, err := NewKitProvider("kit", server.URL+"/v1", "gemini-embedding-2", 2, "", server.Client(), "generation")
	require.NoError(t, err)
	doc, _, err := AssembleDocument(AssembleRequest{
		Owner: Owner{MessageID: 1, BlobHash: testHash}, Media: &MediaInput{MIMEType: "image/png", BlobHash: testHash, Bytes: []byte{1, 2, 3}},
		Context: MessageContext{Body: "private context"}, Policy: ContextPolicy{ExcludeContext: true},
	})
	require.NoError(t, err)
	result, err := p.EmbedDocuments(t.Context(), []DocumentInput{doc})
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, int64(1), result[0].Owner.MessageID)
	_, _, err = p.EmbedQuery(t.Context(), QueryInput{Text: "beach"})
	require.NoError(t, err)
	_, _, err = p.EmbedQuery(t.Context(), QueryInput{Image: &MediaInput{MIMEType: "image/png", Bytes: []byte{4}}})
	require.NoError(t, err)
	require.Len(t, requests, 3)
	assert.Equal(t, []any{map[string]any{"role": "document", "media_type": "image/png", "data": "AQID"}}, requests[0]["inputs"])
	assert.Equal(t, []any{map[string]any{"role": "query", "text": "task: search result | query: beach"}}, requests[1]["inputs"])
	assert.Equal(t, []any{map[string]any{"role": "query", "media_type": "image/png", "data": "BA=="}}, requests[2]["inputs"])
	assert.Equal(t, "generation", p.PolicyFingerprint())
}

func TestKitProviderErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"credentials", 401, ``, ErrProviderUnauthorized},
		{"model", 400, `{"error":{"code":"model_not_found"}}`, ErrProviderConfiguration},
		{"forbidden model", 403, `{"error":{"code":"model_not_found"}}`, ErrProviderConfiguration},
		{"unsupported", 415, `{"error":{"code":"unsupported_content"}}`, ErrProviderConfiguration},
		{"unknown input", 400, ``, ErrProviderRetryable},
		{"input too long", 400, `{"error":{"code":"input_too_long"}}`, ErrProviderRejected},
		{"content policy", 400, `{"error":{"code":"content_policy_violation"}}`, ErrProviderRejected},
		{"rate limit", 429, ``, ErrProviderRetryable},
		{"bad vector", 200, `{"data":[{"index":0,"embedding":[1]}]}`, ErrProviderMalformed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)
			p, err := NewKitProvider("kit", server.URL+"/v1", "model", 2, "", server.Client(), "generation")
			require.NoError(t, err)
			_, _, err = p.EmbedQuery(t.Context(), QueryInput{Text: "beach"})
			require.ErrorIs(t, err, tt.want)
			if tt.status == 429 {
				delay, ok := ProviderRetryAfter(err)
				assert.True(t, ok)
				assert.Equal(t, 3*time.Second, delay)
			}
		})
	}
}
