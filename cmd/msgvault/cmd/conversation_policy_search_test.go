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
			require.ErrorContains(t, root.Execute(), "include:cold requires --mode=fts")
		}
	}
	assert.Zero(t, calls.Load())
}

func TestConversationPolicyIncludeColdRemoteRoundTrip(t *testing.T) {
	// An older daemon reads include:cold as a search word and finds nothing,
	// so the CLI must refuse before sending the search.
	for _, version := range []string{"3.48.0", "3.49.0"} {
		t.Run(version, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			resetSearchFlags()
			t.Cleanup(resetSearchFlags)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					w.Header().Set("Content-Type", "application/json")
					_, err := w.Write([]byte(`{"status":"ok","api_schema_version":"` + version + `"}`))
					assertions.NoError(err)
					return
				}
				calls.Add(1)
				assertions.Equal("/api/v1/cli/search", r.URL.Path)
				q := search.ParseOR(r.URL.Query().Get("q"))
				assertions.True(q.IncludeCold)
				assertions.Len(q.Alternatives, 2)
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(`{"results":[]}`))
				assertions.NoError(err)
			}))
			t.Cleanup(srv.Close)
			cfg := testConfigValue()
			cfg.Remote.URL = srv.URL
			cfg.Remote.AllowInsecure = true
			root := newTestRootCmd()
			root.AddCommand(searchCmd)
			root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			root.SetArgs([]string{"search", "subject:needle | subject:other", "--include-cold", "--json"})
			err := root.Execute()
			if version == "3.49.0" {
				requirements.NoError(err)
				assertions.Equal(int32(1), calls.Load())
				return
			}
			requirements.ErrorContains(err, "include:cold requires daemon API schema 3.49.0")
			assertions.Zero(calls.Load())
		})
	}
}
