package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func TestConversationPolicyIncludeColdNeverReranksCold(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	cfg := testConfigValue()
	cfg.Remote.URL = srv.URL
	cfg.Remote.AllowInsecure = true
	for _, mode := range []string{"vector", "hybrid"} {
		for _, args := range [][]string{{"search", "needle include:cold", "--mode", mode}, {"search", "needle", "--include-cold", "--mode", mode}} {
			resetSearchFlags()
			t.Cleanup(resetSearchFlags)
			root := newTestRootCmd()
			root.AddCommand(searchCmd)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			root.SetArgs(args)
			require.ErrorContains(t, root.Execute(), "lexical")
		}
	}
	assert.Zero(t, calls.Load())
}

func TestConversationPolicyIncludeColdRemoteRoundTrip(t *testing.T) {
	resetSearchFlags()
	t.Cleanup(resetSearchFlags)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{"status":"ok","api_schema_version":"3.19.0"}`))
			assert.NoError(t, err)
			return
		}
		calls.Add(1)
		assert.Equal(t, "/api/v1/cli/search", r.URL.Path)
		q := search.ParseOR(r.URL.Query().Get("q"))
		assert.True(t, q.IncludeCold)
		assert.Len(t, q.Alternatives, 2)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"results":[]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)
	cfg := testConfigValue()
	cfg.Remote.URL = srv.URL
	cfg.Remote.AllowInsecure = true
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	root.SetArgs([]string{"search", "subject:needle | subject:other", "--include-cold", "--json"})
	require.NoError(t, root.Execute())
	assert.Equal(t, int32(1), calls.Load())
}
