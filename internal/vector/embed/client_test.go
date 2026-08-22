package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

type requestConsentState struct{ active atomic.Bool }

func (s *requestConsentState) HasActivePersonSemanticEmbeddingConsent(
	context.Context, string,
) (bool, error) {
	return s.active.Load(), nil
}

func newSemanticRequestGate(endpoint string) (*requestConsentState, vector.SemanticPersonEmbeddingGate) {
	consent := &requestConsentState{}
	consent.active.Store(true)
	config := vector.Config{
		Enabled: true,
		Embeddings: vector.EmbeddingsConfig{
			Endpoint: endpoint, Model: "m", Dimension: 1,
		},
		People: vector.PeopleConfig{
			Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
		},
	}
	return consent, vector.NewExactSemanticPersonEmbeddingGate(
		func() (vector.Config, error) { return config, nil }, consent,
	)
}

// writeEmbeddings writes an OpenAI-compatible embeddings response using the
// provided vectors. It panics on encoding failure; that never happens for
// fixed test payloads.
func writeEmbeddings(t *testing.T, w http.ResponseWriter, vecs [][]float32) {
	t.Helper()
	type item struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	}
	payload := struct {
		Data  []item `json:"data"`
		Model string `json:"model"`
	}{Model: "test-model"}
	for i, v := range vecs {
		payload.Data = append(payload.Data, item{Embedding: v, Index: i})
	}
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(payload), "encode response")
}

func decodeRequest(t *testing.T, r *http.Request) embeddingRequest {
	t.Helper()
	var req embeddingRequest
	require.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode request")
	return req
}

func newRecordingOpenAIClient(t *testing.T) (*Client, *[][]string) {
	t.Helper()
	calls := &[][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		*calls = append(*calls, append([]string(nil), req.Input...))
		vectors := make([][]float32, len(req.Input))
		for i := range req.Input {
			vectors[i] = []float32{float32(i + 1)}
		}
		writeEmbeddings(t, w, vectors)
	}))
	t.Cleanup(srv.Close)
	return NewClient(Config{Endpoint: srv.URL, Model: "test-model", Dimension: 1}), calls
}

// TestClient_EmbedDocumentsFlattensWithoutChangingOrder catches adapters that
// reorder chunks or lose document boundaries around a flat provider request.
func TestClient_EmbedDocumentsFlattensWithoutChangingOrder(t *testing.T) {
	client, calls := newRecordingOpenAIClient(t)
	docs := []DocumentInput{{Chunks: []string{"a", "b"}}, {Chunks: []string{"c"}}}

	got, err := client.EmbedDocuments(context.Background(), docs)

	require.NoError(t, err)
	assert.Equal(t, [][]string{{"a", "b", "c"}}, *calls)
	assert.Equal(t, [][][]float32{{{1}, {2}}, {{3}}}, got)
}

// TestClient_EmbedQueryUsesSingleInput catches query adapters that batch or
// discard the one vector returned by the flat provider request.
func TestClient_EmbedQueryUsesSingleInput(t *testing.T) {
	client, calls := newRecordingOpenAIClient(t)

	got, err := client.EmbedQuery(context.Background(), "find this")

	require.NoError(t, err)
	assert.Equal(t, [][]string{{"find this"}}, *calls)
	assert.Equal(t, []float32{1}, got)
}

// TestClientBeforeRequestFencesEveryRetryAttempt catches a consent check that
// runs once around the logical embedding operation instead of immediately
// before every real HTTP attempt. Query retries are included because curated
// person search uses the same provider client surface as document embedding.
func TestClientBeforeRequestFencesEveryRetryAttempt(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(context.Context, *Client) error
	}{
		{
			name: "documents",
			run: func(ctx context.Context, client *Client) error {
				_, err := client.EmbedDocuments(ctx, []DocumentInput{{Chunks: []string{"curated person"}}})
				return err
			},
		},
		{
			name: "query",
			run: func(ctx context.Context, client *Client) error {
				_, err := client.EmbedQuery(ctx, "curated person query")
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			consent, gate := newSemanticRequestGate("https://embedding.example.test/v1")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				consent.active.Store(false)
				w.Header().Set("Retry-After", "0")
				http.Error(w, "rate limited", http.StatusTooManyRequests)
			}))
			t.Cleanup(server.Close)

			client := NewClient(Config{
				Endpoint: server.URL, Model: "m", Dimension: 1, MaxRetries: 3,
				BeforeRequest: gate.Check,
			})

			err := test.run(t.Context(), client)

			require.ErrorIs(t, err, vector.ErrSemanticPersonEmbeddingConsentRequired)
			assert.Equal(t, int32(1), requests.Load(),
				"revocation after the first 429 must fence the retry")
		})
	}
}

// TestClientBeforeRequestRejectsProviderRedirects catches gated person clients
// that let net/http replay a document or query body to a provider-selected URL.
func TestClientBeforeRequestRejectsProviderRedirects(t *testing.T) {
	operations := []struct {
		name string
		run  func(context.Context, *Client) error
	}{
		{
			name: "documents",
			run: func(ctx context.Context, client *Client) error {
				_, err := client.EmbedDocuments(ctx, []DocumentInput{{Chunks: []string{"curated person"}}})
				return err
			},
		},
		{
			name: "query",
			run: func(ctx context.Context, client *Client) error {
				_, err := client.EmbedQuery(ctx, "curated person query")
				return err
			},
		},
	}
	statuses := []struct {
		name string
		code int
	}{
		{name: "307", code: http.StatusTemporaryRedirect},
		{name: "308", code: http.StatusPermanentRedirect},
	}
	destinations := []struct {
		name        string
		crossOrigin bool
	}{
		{name: "same_origin"},
		{name: "cross_origin", crossOrigin: true},
	}

	for _, operation := range operations {
		for _, status := range statuses {
			for _, destination := range destinations {
				t.Run(operation.name+"/"+status.name+"/"+destination.name, func(t *testing.T) {
					var originRequests atomic.Int32
					var targetRequests atomic.Int32
					redirectLocation := "/redirected"
					if destination.crossOrigin {
						target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							targetRequests.Add(1)
							writeEmbeddings(t, w, [][]float32{{1}})
						}))
						t.Cleanup(target.Close)
						redirectLocation = target.URL + "/redirected"
					}
					origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/embeddings":
							originRequests.Add(1)
							w.Header().Set("Location", redirectLocation)
							w.WriteHeader(status.code)
						case "/redirected":
							targetRequests.Add(1)
							writeEmbeddings(t, w, [][]float32{{1}})
						default:
							http.NotFound(w, r)
						}
					}))
					t.Cleanup(origin.Close)
					_, gate := newSemanticRequestGate("https://embedding.example.test/v1")
					client := NewClient(Config{
						Endpoint: origin.URL, Model: "m", Dimension: 1, MaxRetries: 3,
						BeforeRequest: gate.Check,
					})

					err := operation.run(t.Context(), client)

					assert := assert.New(t)
					require.ErrorIs(t, err, ErrEmbeddingProviderRedirect)
					assert.Equal("embedding provider redirects are not allowed", err.Error())
					assert.Equal(int32(1), originRequests.Load(), "redirect responses must not be retried")
					assert.Zero(targetRequests.Load(), "person text must not reach the redirect target")
				})
			}
		}
	}
}

// TestClientWithoutBeforeRequestFollowsProviderRedirects protects the message
// client composition, which intentionally retains net/http's redirect behavior.
func TestClientWithoutBeforeRequestFollowsProviderRedirects(t *testing.T) {
	for _, status := range []struct {
		name string
		code int
	}{
		{name: "307", code: http.StatusTemporaryRedirect},
		{name: "308", code: http.StatusPermanentRedirect},
	} {
		t.Run(status.name, func(t *testing.T) {
			var originRequests atomic.Int32
			var targetRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/embeddings":
					originRequests.Add(1)
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(status.code)
				case "/redirected":
					targetRequests.Add(1)
					request := decodeRequest(t, r)
					assert.Equal(t, []string{"message text"}, request.Input)
					writeEmbeddings(t, w, [][]float32{{1}})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			client := NewClient(Config{
				Endpoint: server.URL, Model: "m", Dimension: 1, MaxRetries: 1,
			})

			vector, err := client.EmbedQuery(t.Context(), "message text")

			assert := assert.New(t)
			require.NoError(t, err)
			assert.Equal([]float32{1}, vector)
			assert.Equal(int32(1), originRequests.Load())
			assert.Equal(int32(1), targetRequests.Load())
		})
	}
}

func TestClient_Embed_Success(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/embeddings", r.URL.Path)
		assert.Equal("application/json", r.Header.Get("Content-Type"))
		req := decodeRequest(t, r)
		assert.Len(req.Input, 2)
		assert.Equal("test-model", req.Model)
		writeEmbeddings(t, w, [][]float32{
			{0.1, 0.2, 0.3},
			{0.4, 0.5, 0.6},
		})
	}))
	defer srv.Close()

	c := NewClient(Config{
		Endpoint:  srv.URL,
		Model:     "test-model",
		Dimension: 3,
	})
	vecs, err := c.Embed(context.Background(), []string{"hello", "world"})
	require.NoError(err, "Embed")
	require.Len(vecs, 2)
	for i, v := range vecs {
		assert.Len(v, 3, "vecs[%d]", i)
	}
	assert.InDelta(float32(0.1), vecs[0][0], 1e-6)
	assert.InDelta(float32(0.6), vecs[1][2], 1e-6)
}

func TestClient_Embed_DimensionMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEmbeddings(t, w, [][]float32{{0.1, 0.2}})
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3})
	_, err := c.Embed(context.Background(), []string{"a"})
	require.Error(t, err, "expected dimension mismatch error")
	assert.ErrorContains(t, err, "dimension mismatch")
}

func TestClient_Embed_Retries5xx(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		writeEmbeddings(t, w, [][]float32{{0.1, 0.2, 0.3}})
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3, MaxRetries: 3})
	vecs, err := c.Embed(context.Background(), []string{"a"})
	require.NoError(err, "Embed")
	assert.Equal(int32(3), attempts.Load())
	require.Len(vecs, 1)
	assert.Len(vecs[0], 3)
}

func TestClient_Embed_Does_Not_Retry_4xx(t *testing.T) {
	assert := assert.New(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"No models loaded"}`))
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3, MaxRetries: 5})
	_, err := c.Embed(context.Background(), []string{"a"})
	require.Error(t, err, "expected error for 4xx")
	assert.Equal(int32(1), attempts.Load(), "no retry on 4xx")
	require.ErrorContains(t, err, "400")
	assert.ErrorContains(err, "No models loaded")
}

func TestClient_Embed_AuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeEmbeddings(t, w, [][]float32{{1, 2, 3}})
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, APIKey: "secret-token", Model: "m", Dimension: 3})
	_, err := c.Embed(context.Background(), []string{"a"})
	require.NoError(t, err, "Embed")
	assert.Equal(t, "Bearer secret-token", gotAuth)
}

func TestClient_Embed_EmptyInput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		writeEmbeddings(t, w, nil)
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3})

	vecs, err := c.Embed(context.Background(), nil)
	require.NoError(err, "nil input")
	assert.Nil(vecs)

	vecs, err = c.Embed(context.Background(), []string{})
	require.NoError(err, "empty input")
	assert.Nil(vecs)

	assert.Equal(int32(0), attempts.Load(), "no HTTP call for empty input")
}

func TestClient_Embed_GivesUpAfterMaxRetries(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3, MaxRetries: 2})
	_, err := c.Embed(context.Background(), []string{"a"})
	require.Error(t, err, "expected error after exhausting retries")
	assert.Equal(t, int32(2), attempts.Load())
	assert.ErrorContains(t, err, "giving up")
}

func TestClient_Embed_ContextCanceledDuringBackoff(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3, MaxRetries: 10})

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after start so we hit the backoff wait.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := c.Embed(ctx, []string{"a"})
	require.Error(t, err, "expected error from canceled context")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestClient_Embed_MissingIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Two response items preserve the expected raw count, but both target
		// index 0 so normalized index 1 remains missing.
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		payload := struct {
			Data  []item `json:"data"`
			Model string `json:"model"`
		}{
			Data: []item{
				{Embedding: []float32{0.1, 0.2, 0.3}, Index: 0},
				{Embedding: []float32{0.4, 0.5, 0.6}, Index: 0},
			},
			Model: "m",
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			http.Error(w, "encode response", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3})
	_, err := c.Embed(context.Background(), []string{"a", "b"})
	require.Error(t, err, "expected missing embedding error")
	assert.ErrorContains(t, err, "missing embedding at index 1")
}

// TestClient_Embed_Retries429 verifies 429 Too Many Requests is
// treated as transient and retried rather than failing immediately.
func TestClient_Embed_Retries429(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 2 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		writeEmbeddings(t, w, [][]float32{{0.1, 0.2, 0.3}})
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3, MaxRetries: 3})
	vecs, err := c.Embed(context.Background(), []string{"a"})
	require.NoError(err, "Embed")
	assert.Equal(int32(2), attempts.Load(), "retry after 429")
	require.Len(vecs, 1)
	assert.Len(vecs[0], 3)
}

// TestClient_Embed_HonorsRetryAfterOverridesBackoff verifies that a
// long Retry-After value stretches the retry wait past the default
// exponential backoff. Cancelling the context mid-wait must return
// a context-cancel error rather than racing the default-backoff
// deadline.
func TestClient_Embed_HonorsRetryAfterOverridesBackoff(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "30") // much longer than default 200ms
		http.Error(w, "rl", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3, MaxRetries: 3})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := c.Embed(ctx, []string{"a"})
	elapsed := time.Since(start)
	require.ErrorIs(t, err, context.Canceled)
	// Should be interrupted at ~100ms by the cancel, well before
	// 30s. A test failure here would mean Retry-After wasn't
	// honored and the default backoff completed first.
	assert.Less(t, elapsed, 500*time.Millisecond, "cancel during Retry-After wait")
	// One attempt plus possibly a second before cancel; never
	// enough to finish the Retry-After window.
	assert.LessOrEqual(t, attempts.Load(), int32(2), "Retry-After should extend the wait")
}

// TestClient_Embed_RetriesTruncatedBody verifies a truncated JSON
// response is treated as transient. Mid-stream cutoffs are common
// when the server hits a deadline, and the old code failed them
// outright.
func TestClient_Embed_RetriesTruncatedBody(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n < 2 {
			// Write a prefix then cut the connection mid-JSON.
			_, _ = w.Write([]byte(`{"data": [{"embedd`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if h, ok := w.(http.Hijacker); ok {
				conn, _, err := h.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		writeEmbeddings(t, w, [][]float32{{0.1, 0.2, 0.3}})
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3, MaxRetries: 3})
	vecs, err := c.Embed(context.Background(), []string{"a"})
	require.NoError(t, err, "Embed")
	assert.Equal(t, int32(2), attempts.Load(), "retry after truncated body")
	assert.Len(t, vecs, 1)
}

// TestClient_parseRetryAfter covers the Retry-After formats (seconds,
// HTTP-date, unparseable) and the cap that protects against absurd
// server-supplied values. The (Duration, bool) return distinguishes
// "Retry-After: 0" (parsed = true, immediate retry) from "missing or
// unparseable" (parsed = false, use default backoff).
func TestClient_parseRetryAfter(t *testing.T) {
	assert := assert.New(t)
	cases := []struct {
		in      string
		wantDur time.Duration
		wantOk  bool
	}{
		{"", 0, false},
		{"   ", 0, false},
		{"abc", 0, false},
		{"-5", 0, false},
		{"0", 0, true}, // explicit immediate retry
		{"2", 2 * time.Second, true},
	}
	for _, c := range cases {
		gotDur, gotOk := parseRetryAfter(c.in)
		assert.Equalf(c.wantDur, gotDur, "parseRetryAfter(%q) duration", c.in)
		assert.Equalf(c.wantOk, gotOk, "parseRetryAfter(%q) ok", c.in)
	}
	// Cap: 7200 seconds is capped to 1 hour.
	got, ok := parseRetryAfter("7200")
	assert.Equal(time.Hour, got, "parseRetryAfter(7200) duration")
	assert.True(ok, "parseRetryAfter(7200) ok")
	// HTTP-date: one second in the future is a non-zero positive
	// duration well under the cap.
	future := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
	got, ok = parseRetryAfter(future)
	assert.True(ok, "parseRetryAfter(%q) ok", future)
	assert.Greater(got, time.Duration(0), "parseRetryAfter(%q) duration > 0", future)
	assert.LessOrEqual(got, time.Hour, "parseRetryAfter(%q) duration <= 1h", future)
}

// TestClient_Embed_RetryAfterZero_RetriesImmediately regresses the
// bug where Retry-After: 0 was indistinguishable from "no override"
// and fell back to exponential backoff. With the (Duration, bool)
// return, an explicit zero must take precedence and retry without
// waiting. We assert by measuring elapsed time across two attempts:
// the second attempt must start far sooner than the default
// 200ms backoff for attempt #1.
func TestClient_Embed_RetryAfterZero_RetriesImmediately(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeEmbeddings(t, w, [][]float32{{1, 0, 0, 0}})
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 4, MaxRetries: 3})
	start := time.Now()
	vecs, err := c.Embed(context.Background(), []string{"hello"})
	elapsed := time.Since(start)
	require.NoError(err, "Embed")
	require.Len(vecs, 1)
	// Default backoff for attempt #1 is 1<<1 * 100ms = 200ms.
	// Retry-After: 0 should drop that to ~0. Allow generous slack
	// (50ms) for HTTP roundtrips on slow CI.
	assert.Less(elapsed, 100*time.Millisecond, "Retry-After: 0 should bypass exponential backoff")
	assert.Equal(2, calls)
}

func TestClient_Embed_4xxIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"Invalid input"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(Config{
		Endpoint: srv.URL, Model: "m", Dimension: 4, MaxRetries: 3,
	})
	_, err := c.Embed(context.Background(), []string{"hello"})
	require.Error(t, err, "expected error on 400")
	require.ErrorIs(t, err, ErrPermanent4xx)
	// Existing contract: body must still be in the message.
	assert.ErrorContains(t, err, "Invalid input")
}

func TestClient_Embed_5xxNotPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(Config{
		Endpoint: srv.URL, Model: "m", Dimension: 4, MaxRetries: 2,
	})
	_, err := c.Embed(context.Background(), []string{"hello"})
	require.Error(t, err, "expected error after retries exhausted")
	assert.NotErrorIs(t, err, ErrPermanent4xx, "5xx should NOT match ErrPermanent4xx")
}

func TestClient_Embed_429NotPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(Config{
		Endpoint: srv.URL, Model: "m", Dimension: 4, MaxRetries: 2,
	})
	_, err := c.Embed(context.Background(), []string{"hello"})
	require.Error(t, err, "expected error after retries exhausted")
	assert.NotErrorIs(t, err, ErrPermanent4xx, "429 should NOT match ErrPermanent4xx")
}

func TestClient_Embed_InvalidIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return index 5 for a 1-input request.
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		payload := struct {
			Data  []item `json:"data"`
			Model string `json:"model"`
		}{
			Data:  []item{{Embedding: []float32{0.1, 0.2, 0.3}, Index: 5}},
			Model: "m",
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(payload), "encode")
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3})
	_, err := c.Embed(context.Background(), []string{"a"})
	require.Error(t, err, "expected invalid index error")
	assert.ErrorContains(t, err, "invalid index")
}

// TestClient_Embed_RejectsExtraOrDuplicateResponseItems catches provider
// responses whose extra data items are hidden by normalized index slots.
func TestClient_Embed_RejectsExtraOrDuplicateResponseItems(t *testing.T) {
	type item struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	}
	tests := []struct {
		name string
		data []item
	}{
		{
			name: "duplicate index",
			data: []item{
				{Embedding: []float32{0.1, 0.2, 0.3}, Index: 0},
				{Embedding: []float32{0.4, 0.5, 0.6}, Index: 0},
			},
		},
		{
			name: "extra index",
			data: []item{
				{Embedding: []float32{0.1, 0.2, 0.3}, Index: 0},
				{Embedding: []float32{0.4, 0.5, 0.6}, Index: 1},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				payload := struct {
					Data  []item `json:"data"`
					Model string `json:"model"`
				}{Data: test.data, Model: "m"}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(payload); err != nil {
					http.Error(w, "encode response", http.StatusInternalServerError)
				}
			}))
			t.Cleanup(srv.Close)

			client := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3})
			_, err := client.Embed(context.Background(), []string{"a"})

			require.Error(t, err)
			assert.ErrorContains(t, err, "response count mismatch: got 2, expected 1")
		})
	}
}
