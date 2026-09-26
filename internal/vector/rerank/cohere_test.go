package rerank

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The provider is an external HTTP contract, so these tests serve it from
// httptest. The success body is a response captured from OpenRouter's
// /api/v1/rerank for cohere/rerank-4-pro.

var fixtureCandidates = []string{
	"Votre colis sera livré mardi entre 9h et 12h.",
	"Reminder: team lunch on Friday at noon.",
	"Rechnung Nr. 4471 ist fällig.",
}

func newTestClient(t *testing.T, handler http.HandlerFunc, opts CohereOptions) *CohereClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	if opts.Endpoint == "" {
		opts.Endpoint = server.URL + "/api/v1"
	}
	if opts.Model == "" {
		opts.Model = "cohere/rerank-4-pro"
	}
	client, err := NewCohereClient(opts)
	require.NoError(t, err)
	return client
}

func writeJSONResponse(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

func TestCohereClientScoresCandidatesInRequestOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture, err := os.ReadFile("testdata/openrouter_rerank_response.json")
	require.NoError(err)
	var got struct {
		path, auth, contentType string
		body                    map[string]any
	}
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.contentType = r.Header.Get("Content-Type")
		assert.NoError(json.NewDecoder(r.Body).Decode(&got.body))
		writeJSONResponse(w, string(fixture))
	}, CohereOptions{APIKey: "sk-test"})

	result, err := client.Rerank(context.Background(), Request{
		Query: "quand arrive le colis ?", Candidates: fixtureCandidates,
	})
	require.NoError(err)
	assert.Equal("/api/v1/rerank", got.path)
	assert.Equal("Bearer sk-test", got.auth)
	assert.Equal("application/json", got.contentType)
	assert.Equal(map[string]any{
		"model":     "cohere/rerank-4-pro",
		"query":     "quand arrive le colis ?",
		"documents": []any{fixtureCandidates[0], fixtureCandidates[1], fixtureCandidates[2]},
		"top_n":     float64(3),
	}, got.body)
	// The provider lists results by descending score; Scores follows the
	// request's candidate order.
	assert.InDeltaSlice([]float64{0.93982506, 0.5093739, 0.5560139}, result.Scores, 1e-9)
	assert.Equal(1, result.Usage.Requests)
	assert.False(result.Usage.Complete, "search-unit billing reports no token count")

	order, err := Order(result.Scores)
	require.NoError(err)
	assert.Equal([]int{0, 2, 1}, order)
}

func TestCohereClientReportsTokenUsage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(w,
			`{"results":[{"index":0,"relevance_score":0.7}],"usage":{"total_tokens":256,"cost":0.00005}}`)
	}, CohereOptions{})

	result, err := client.Rerank(context.Background(), Request{Query: "q", Candidates: []string{"a"}})
	require.NoError(err)
	require.NotNil(result.Usage.InputTokens)
	assert.Equal(int64(256), *result.Usage.InputTokens)
	assert.True(result.Usage.Complete)
}

func TestCohereClientOmitsAuthorizationWithoutKey(t *testing.T) {
	var auth atomic.Value
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		writeJSONResponse(w, `{"results":[{"index":0,"relevance_score":0.5}]}`)
	}, CohereOptions{})

	_, err := client.Rerank(context.Background(), Request{Query: "q", Candidates: []string{"a"}})
	require.NoError(t, err)
	assert.Empty(t, auth.Load())
}

func TestCohereClientRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		category    string
		message     string
	}{
		{
			name: "provider error keeps provider message for the log", status: http.StatusBadRequest,
			body:     `{"error":{"message":"Model cohere/nope does not exist","code":400}}`,
			category: FailureProviderStatus, message: "HTTP 400: Model cohere/nope does not exist",
		},
		{
			name: "cohere error shape", status: http.StatusUnauthorized,
			body:     `{"message":"invalid api token"}`,
			category: FailureProviderStatus, message: "HTTP 401: invalid api token",
		},
		{
			name: "missing result", status: http.StatusOK,
			body:     `{"results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.4}]}`,
			category: FailureInvalidResponse, message: "provider scored 2 of 3 candidates",
		},
		{
			name: "repeated index", status: http.StatusOK,
			body:     `{"results":[{"index":0,"relevance_score":0.5},{"index":0,"relevance_score":0.4},{"index":2,"relevance_score":0.1}]}`,
			category: FailureInvalidResponse, message: "out of range or repeated",
		},
		{
			name: "index out of range", status: http.StatusOK,
			body:     `{"results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.4},{"index":3,"relevance_score":0.1}]}`,
			category: FailureInvalidResponse, message: "out of range or repeated",
		},
		{
			name: "score above one", status: http.StatusOK,
			body:     `{"results":[{"index":0,"relevance_score":4.2},{"index":1,"relevance_score":0.4},{"index":2,"relevance_score":0.1}]}`,
			category: FailureInvalidResponse, message: "not in [0,1]",
		},
		{
			name: "missing score", status: http.StatusOK,
			body:     `{"results":[{"index":0},{"index":1,"relevance_score":0.4},{"index":2,"relevance_score":0.1}]}`,
			category: FailureInvalidResponse, message: "lacks an index or score",
		},
		{
			name: "not JSON", status: http.StatusOK, contentType: "text/html",
			body:     `<html>gateway</html>`,
			category: FailureInvalidResponse, message: "non-JSON",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				contentType := tt.contentType
				if contentType == "" {
					contentType = "application/json"
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}, CohereOptions{})

			_, err := client.Rerank(context.Background(), Request{Query: "q", Candidates: fixtureCandidates})
			require.Error(t, err)
			assert.Equal(t, tt.category, FailureCategory(err))
			assert.Contains(t, err.Error(), tt.message)
		})
	}
}

func TestCohereClientTimesOut(t *testing.T) {
	release := make(chan struct{})
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-release
	}, CohereOptions{Timeout: 50 * time.Millisecond})
	// Registered after the server's own cleanup so it runs first: the
	// server's Close waits for this handler to return.
	t.Cleanup(func() { close(release) })

	_, err := client.Rerank(context.Background(), Request{Query: "q", Candidates: []string{"a"}})
	require.Error(t, err)
	assert.Equal(t, FailureTimeout, FailureCategory(err))
}

func TestCohereClientDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	t.Cleanup(target.Close)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/rerank", http.StatusTemporaryRedirect)
	}, CohereOptions{APIKey: "sk-test"})

	_, err := client.Rerank(context.Background(), Request{Query: "q", Candidates: []string{"a"}})
	require.Error(t, err)
	assert.Equal(t, FailureProviderStatus, FailureCategory(err))
	assert.False(t, redirected.Load(), "the credential must not be replayed to another origin")
}

func TestCohereClientBoundsRequests(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSONResponse(w, `{"results":[]}`)
	}, CohereOptions{})

	result, err := client.Rerank(context.Background(), Request{Query: "q"})
	require.NoError(err)
	assert.Empty(result.Scores)

	_, err = client.Rerank(context.Background(), Request{Query: " ", Candidates: []string{"a"}})
	assert.Equal(FailureRequest, FailureCategory(err))

	tooMany := make([]string, MaxCandidates+1)
	for i := range tooMany {
		tooMany[i] = "text"
	}
	_, err = client.Rerank(context.Background(), Request{Query: "q", Candidates: tooMany})
	assert.Equal(FailureRequest, FailureCategory(err))
	assert.Equal(int32(0), calls.Load(), "invalid requests never reach the provider")
}

func TestNewCohereClientValidatesOptions(t *testing.T) {
	tests := []struct {
		name string
		opts CohereOptions
		want string
	}{
		{"missing endpoint", CohereOptions{Model: "m"}, "http or https URL"},
		{"unsupported scheme", CohereOptions{Endpoint: "ftp://example.com", Model: "m"}, "http or https URL"},
		{"credentials in URL", CohereOptions{Endpoint: "https://user:pw@example.com/v1", Model: "m"}, "must not contain"},
		{"query in URL", CohereOptions{Endpoint: "https://example.com/v1?key=x", Model: "m"}, "must not contain"},
		{"missing model", CohereOptions{Endpoint: "https://example.com/v1"}, "model is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewCohereClient(tt.opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}

	client, err := NewCohereClient(CohereOptions{Endpoint: "https://openrouter.ai/api/v1/", Model: " cohere/rerank-4-pro "})
	require.NoError(t, err)
	assert.Equal(t, "cohere/rerank-4-pro", client.Model())
	assert.Equal(t, "https://openrouter.ai/api/v1/rerank", client.url)
}
