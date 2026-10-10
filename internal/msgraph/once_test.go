package msgraph

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetJSONOnceBoundsAndDoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"transient", http.StatusServiceUnavailable, `{}`},
		{"oversized", http.StatusOK, strings.Repeat(" ", 65)},
		{"invalid JSON", http.StatusOK, `{"id":"one"} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodGet, r.Method)
				w.WriteHeader(tc.status)
				_, err := w.Write([]byte(tc.body))
				assert.NoError(t, err)
			}))
			defer server.Close()
			client := NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
			var body map[string]any
			err := client.GetJSONOnce(t.Context(), "/me", &body, 64)
			require.Error(t, err)
			if tc.name == "oversized" {
				require.ErrorIs(t, err, ErrTooLarge)
			}
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestGraphOnceRejectsRedirects(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			var calls, redirected atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/other" {
					redirected.Add(1)
					_, err := w.Write([]byte(`{}`))
					assert.NoError(t, err)
					return
				}
				calls.Add(1)
				w.Header().Set("Location", "/other")
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer server.Close()
			client := NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
			var err error
			if method == http.MethodGet {
				var body map[string]any
				err = client.GetJSONOnce(t.Context(), "/me", &body, 64)
			} else {
				err = client.PatchIfMatch(t.Context(), "/me/messages/one", map[string]any{"categories": []string{"Next"}}, `W/"v1"`)
			}
			require.Error(t, err)
			assert.Equal(t, int32(1), calls.Load())
			assert.Zero(t, redirected.Load())
		})
	}
}

func TestGetJSONOnceRejectsInvalidCapBeforeIO(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, err := w.Write([]byte(`{}`))
		assert.NoError(t, err)
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
	for _, cap := range []int64{-1, 0, (1 << 20) + 1} {
		var body map[string]any
		require.Error(t, client.GetJSONOnce(t.Context(), "/me", &body, cap))
	}
	assert.Zero(t, calls.Load())
}

func TestPatchIfMatchAcceptsOversizedSuccessForReadback(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(map[bool]string{false: "streamed", true: "declared"}[declared], func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodPatch, r.Method)
				assert.Equal(t, `W/"v1"`, r.Header.Get("If-Match"))
				if declared {
					w.Header().Set("Content-Length", strconv.Itoa((1<<20)+1))
				}
				w.WriteHeader(http.StatusOK)
				if !declared {
					flusher, ok := w.(http.Flusher)
					if !assert.True(t, ok) {
						return
					}
					flusher.Flush()
				}
				// The bounded client may close before consuming this successful body.
				_, _ = io.WriteString(w, strings.Repeat("x", (1<<20)+1))
			}))
			defer server.Close()
			client := NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
			require.NoError(t, client.PatchIfMatch(t.Context(), "/me/messages/one", map[string]any{"categories": []string{"Next"}}, `W/"v1"`))
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestPatchIfMatchDoesNotTreatTokenCapErrorAsSuccess(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context) (string, error) { return "", ErrTooLarge }, 1000)
	require.ErrorIs(t, client.PatchIfMatch(t.Context(), "/me/messages/one", map[string]any{"categories": []string{"Next"}}, `W/"v1"`), ErrTooLarge)
	assert.Zero(t, calls.Load())
}
