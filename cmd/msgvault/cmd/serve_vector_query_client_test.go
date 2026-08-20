//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
)

// embedRequestRecorder captures the one request a query-time embedding call
// makes, so a test can assert the protocol on the wire rather than the Go
// type of the client.
type embedRequestRecorder struct {
	mu   sync.Mutex
	path string
	body map[string]any
}

func (r *embedRequestRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path = req.URL.Path
	r.body = map[string]any{}
	_ = json.NewDecoder(req.Body).Decode(&r.body)
}

func (r *embedRequestRecorder) seen() (string, map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path, r.body
}

// embedTestServer serves one canned embedding response and records the
// request that asked for it.
func embedTestServer(t *testing.T, response string) (*embedRequestRecorder, string) {
	t.Helper()
	rec := &embedRequestRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return rec, srv.URL + "/v1"
}

// queryClientTestConfig is a minimal vector config for the query-side client,
// defaults applied the way config load applies them.
func queryClientTestConfig(endpoint string, format vector.EmbeddingAPIFormat, model string) vector.Config {
	var c vector.Config
	c.Enabled = true
	c.Embeddings.Endpoint = endpoint
	c.Embeddings.Model = model
	c.Embeddings.APIFormat = format
	c.Embeddings.Dimension = 3
	c.ApplyDefaults()
	return c
}

// TestNewQueryEmbeddingClient_VoyageContextualUsesContextualQueryRole pins the
// contract a Voyage-contextual eval or search run depends on: the query goes to
// the contextual endpoint, in the nested request shape, tagged with the query
// role. Constructing the OpenAI-compatible client for this config instead —
// which is what the eval command used to do unconditionally — posts a flat
// {"input": [...]} body to /v1/embeddings with no input_type, so the query
// vector would come from a different endpoint and a different role than the
// documents it is compared against, if the request succeeded at all.
func TestNewQueryEmbeddingClient_VoyageContextualUsesContextualQueryRole(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	rec, endpoint := embedTestServer(t, `{"data":[{"index":0,"data":[{"index":0,"embedding":[0.25,0.5,0.75]}]}]}`)
	client, err := newQueryEmbeddingClient(
		queryClientTestConfig(endpoint, vector.APIFormatVoyageContextual, "voyage-context-4"))
	require.NoError(err)

	vec, err := client.EmbedQuery(context.Background(), "who signed the lease?")
	require.NoError(err)
	assert.Equal([]float32{0.25, 0.5, 0.75}, vec)

	path, body := rec.seen()
	assert.Equal("/v1/contextualizedembeddings", path, "the contextual endpoint, not /embeddings")
	assert.Equal("query", body["input_type"], "a query must be embedded in the query role, not the document role")
	assert.Equal([]any{[]any{"who signed the lease?"}}, body["inputs"], "the contextual request nests chunks per document")
}

// TestNewQueryEmbeddingClient_DefaultFormatStaysOpenAICompatible pins the other
// half: an omitted api_format is still the OpenAI-compatible path, unchanged.
func TestNewQueryEmbeddingClient_DefaultFormatStaysOpenAICompatible(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	rec, endpoint := embedTestServer(t, `{"data":[{"index":0,"embedding":[0.25,0.5,0.75]}]}`)
	client, err := newQueryEmbeddingClient(queryClientTestConfig(endpoint, "", "bge-m3"))
	require.NoError(err)

	vec, err := client.EmbedQuery(context.Background(), "who signed the lease?")
	require.NoError(err)
	assert.Equal([]float32{0.25, 0.5, 0.75}, vec)

	path, body := rec.seen()
	assert.Equal("/v1/embeddings", path)
	assert.Equal([]any{"who signed the lease?"}, body["input"])
	assert.NotContains(body, "input_type", "the OpenAI-compatible body carries no role")
}

// TestNewQueryEmbeddingClient_RejectsFormatsItCannotBuild proves the selector
// fails loudly on a format it has no client for, naming the offending value,
// rather than silently falling back to the OpenAI-compatible client and
// scoring a protocol mismatch as retrieval quality.
func TestNewQueryEmbeddingClient_RejectsFormatsItCannotBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	_, err := newQueryEmbeddingClient(queryClientTestConfig("http://127.0.0.1:1/v1", "voyage", "voyage-context-4"))
	require.Error(err, "an api_format with no client must not fall back")
	assert.Contains(err.Error(), `"voyage"`, "the error names the unsupported value")

	// A contextual format with a model the contextual endpoint does not serve
	// is the same class of silent mismatch, and fails the same way.
	_, err = newQueryEmbeddingClient(
		queryClientTestConfig("http://127.0.0.1:1/v1", vector.APIFormatVoyageContextual, "voyage-large-4"))
	require.Error(err)
	assert.Contains(err.Error(), "voyage-large-4")
}
