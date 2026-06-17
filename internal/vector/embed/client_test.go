package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

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
	requirepkg.NoError(t, json.NewEncoder(w).Encode(payload), "encode response")
}

func decodeRequest(t *testing.T, r *http.Request) embeddingRequest {
	t.Helper()
	var req embeddingRequest
	requirepkg.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode request")
	return req
}

func TestClient_Embed_Success(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	requirepkg.Error(t, err, "expected dimension mismatch error")
	assertpkg.ErrorContains(t, err, "dimension mismatch")
}

func TestClient_Embed_Retries5xx(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	assert := assertpkg.New(t)
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
	requirepkg.Error(t, err, "expected error for 4xx")
	assert.Equal(int32(1), attempts.Load(), "no retry on 4xx")
	requirepkg.ErrorContains(t, err, "400")
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
	requirepkg.NoError(t, err, "Embed")
	assertpkg.Equal(t, "Bearer secret-token", gotAuth)
}

func TestClient_Embed_EmptyInput(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	requirepkg.Error(t, err, "expected error after exhausting retries")
	assertpkg.Equal(t, int32(2), attempts.Load())
	assertpkg.ErrorContains(t, err, "giving up")
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
	requirepkg.Error(t, err, "expected error from canceled context")
	assertpkg.ErrorIs(t, err, context.Canceled)
}

func TestClient_Embed_MissingIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only index 0 returned for 2-input request.
		writeEmbeddings(t, w, [][]float32{{0.1, 0.2, 0.3}})
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3})
	_, err := c.Embed(context.Background(), []string{"a", "b"})
	requirepkg.Error(t, err, "expected missing embedding error")
	assertpkg.ErrorContains(t, err, "missing embedding at index 1")
}

// TestClient_Embed_Retries429 verifies 429 Too Many Requests is
// treated as transient and retried rather than failing immediately.
func TestClient_Embed_Retries429(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	requirepkg.ErrorIs(t, err, context.Canceled)
	// Should be interrupted at ~100ms by the cancel, well before
	// 30s. A test failure here would mean Retry-After wasn't
	// honored and the default backoff completed first.
	assertpkg.Less(t, elapsed, 500*time.Millisecond, "cancel during Retry-After wait")
	// One attempt plus possibly a second before cancel; never
	// enough to finish the Retry-After window.
	assertpkg.LessOrEqual(t, attempts.Load(), int32(2), "Retry-After should extend the wait")
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
	requirepkg.NoError(t, err, "Embed")
	assertpkg.Equal(t, int32(2), attempts.Load(), "retry after truncated body")
	assertpkg.Len(t, vecs, 1)
}

// TestClient_parseRetryAfter covers the Retry-After formats (seconds,
// HTTP-date, unparseable) and the cap that protects against absurd
// server-supplied values. The (Duration, bool) return distinguishes
// "Retry-After: 0" (parsed = true, immediate retry) from "missing or
// unparseable" (parsed = false, use default backoff).
func TestClient_parseRetryAfter(t *testing.T) {
	assert := assertpkg.New(t)
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	requirepkg.Error(t, err, "expected error on 400")
	requirepkg.ErrorIs(t, err, ErrPermanent4xx)
	// Existing contract: body must still be in the message.
	assertpkg.ErrorContains(t, err, "Invalid input")
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
	requirepkg.Error(t, err, "expected error after retries exhausted")
	assertpkg.NotErrorIs(t, err, ErrPermanent4xx, "5xx should NOT match ErrPermanent4xx")
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
	requirepkg.Error(t, err, "expected error after retries exhausted")
	assertpkg.NotErrorIs(t, err, ErrPermanent4xx, "429 should NOT match ErrPermanent4xx")
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
		assertpkg.NoError(t, json.NewEncoder(w).Encode(payload), "encode")
	}))
	defer srv.Close()

	c := NewClient(Config{Endpoint: srv.URL, Model: "m", Dimension: 3})
	_, err := c.Embed(context.Background(), []string{"a"})
	requirepkg.Error(t, err, "expected invalid index error")
	assertpkg.ErrorContains(t, err, "invalid index")
}
