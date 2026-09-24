//go:build sqlite_vec

package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
)

func TestWriteHybridResultsTableFallsBackToMessageSnippet(t *testing.T) {
	sentAt := time.Date(2026, 8, 18, 14, 47, 41, 0, time.UTC)
	tests := []struct {
		name     string
		result   daemonclient.CLIHybridSearchResult
		contains []string
	}{
		{
			name: "chat hit uses sanitized snippet and display name",
			result: daemonclient.CLIHybridSearchResult{
				ID: 201, SentAt: sentAt,
				Message: query.MessageSummary{ID: 201, FromName: "Carol\x1b[31m Example", Snippet: "how many\n\x1b]0;bad\a songs are unreleased"},
			},
			contains: []string{"Carol Example", "how many songs are unreleased"},
		},
		{
			name: "subject wins and boost marker remains",
			result: daemonclient.CLIHybridSearchResult{
				ID: 202, SentAt: sentAt, FromEmail: "dave@example.com", Subject: "Tour dates", SubjectBoosted: true,
				Message: query.MessageSummary{Snippet: "ignored"},
			},
			contains: []string{"dave@example.com", "Tour dates *"},
		},
		{
			name: "zero Message keeps top-level fields",
			result: daemonclient.CLIHybridSearchResult{
				ID: 203, SentAt: sentAt, FromEmail: "erin@example.com", Subject: "Hello",
			},
			contains: []string{"erin@example.com", "Hello"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, writeHybridResultsTable(&buf, []daemonclient.CLIHybridSearchResult{tt.result}, false))
			lines := strings.Split(buf.String(), "\n")
			require.GreaterOrEqual(t, len(lines), 3)
			for _, want := range tt.contains {
				assert.Contains(t, lines[2], want)
			}
			assert.NotContains(t, buf.String(), "\x1b")
		})
	}
}

func TestWriteHybridResultsTableExplainKeepsScores(t *testing.T) {
	rrf, bm25, vec := 0.5, 1.25, 0.75
	result := daemonclient.CLIHybridSearchResult{
		ID: 204, SentAt: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		FromEmail: "frank@example.com", Subject: "Album", SubjectBoosted: true,
		RRFScore: &rrf, BM25Score: &bm25, VectorScore: &vec,
	}
	var buf bytes.Buffer
	require.NoError(t, writeHybridResultsTable(&buf, []daemonclient.CLIHybridSearchResult{result}, true))
	assert.Contains(t, buf.String(), "ID")
	assert.Contains(t, buf.String(), "RRF")
	assert.Contains(t, buf.String(), "BM25")
	assert.Contains(t, buf.String(), "VEC")
	assert.Contains(t, buf.String(), "Album *")
	assert.Contains(t, buf.String(), "0.5000")
	assert.Contains(t, buf.String(), "1.2500")
	assert.Contains(t, buf.String(), "0.7500")
}

func TestSearchCmd_VectorModeUsesLocalDaemonHTTPAndPreservesJSONOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	requests := &atomic.Int32{}
	srv := vectorSearchHTTPDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal("/api/v1/search", r.URL.Path, "path")
		assert.Equal("lunch", r.URL.Query().Get("q"), "query")
		assert.Equal("vector", r.URL.Query().Get("mode"), "mode")
		assert.Equal("true", r.URL.Query().Get("explain"), "explain")
		assert.Equal("alice@example.com", r.URL.Query().Get("account"), "account")
		assert.Equal("sms", r.URL.Query().Get("message_type"), "message_type")
		assert.Equal("50", r.URL.Query().Get("page_size"), "page_size")
		writeVectorSearchResponse(t, w, "alice@example.com", "", 0)
	})
	writeStatsHTTPDaemonRuntime(t, dataDir, srv)

	restore := configureVectorSearchHTTPTest(t, dataDir, true, "")
	defer restore()

	done := captureStdout(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--mode", "vector", "--json",
		"--account", "alice@example.com",
		"--message-type", "sms",
		"lunch",
	})

	err := root.Execute()
	out := done()
	require.NoError(err, "vector search command")

	assert.Equal(1, int(requests.Load()), "search endpoint calls")
	assert.Contains(out, `"returned_count": 1`, "returned_count")
	assert.Contains(out, `"from_email": "alice@example.com"`, "from_email")
	assert.Contains(out, `"boosted": true`, "boosted")
	assert.Contains(out, srv.URL+"/messages/42", "browser URL")
	assert.Contains(out, `"rrf_score": 0.5`, "rrf_score")
	assert.NotContains(out, "bm25_score", "bm25 is hidden without --explain")
	assert.NotContains(out, "vector_score", "vector score is hidden without --explain")
}

func TestSearchCmd_VectorModeCollectionUsesLocalDaemonHTTPAndPreservesBanner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	requests := &atomic.Int32{}
	srv := vectorSearchHTTPDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal("Important", r.URL.Query().Get("collection"), "collection")
		writeVectorSearchResponse(t, w, "alice@example.com", "Important", 2)
	})
	writeStatsHTTPDaemonRuntime(t, dataDir, srv)

	restore := configureVectorSearchHTTPTest(t, dataDir, true, "")
	defer restore()

	doneOut := captureStdout(t)
	doneErr := captureStderr(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--mode", "vector",
		"--collection", "Important",
		"lunch",
	})

	err := root.Execute()
	out := doneOut()
	errOut := doneErr()
	require.NoError(err, "vector collection search command")

	assert.Equal(1, int(requests.Load()), "search endpoint calls")
	assert.Contains(out, "Lunch *", "boosted marker")
	assert.Contains(out, `Showing 1 result (generation #7 active, fingerprint="fake:4")`, "summary")
	assert.Contains(errOut, `Searching collection "Important" (2 accounts)`, "collection banner")
}

func TestSearchCmd_VectorModeUnknownAccountUsesDaemonError(t *testing.T) {
	dataDir := t.TempDir()
	srv := vectorSearchHTTPDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "invalid_scope",
			"message": `no account found for "nobody@nowhere.invalid"`,
		})
	})
	writeStatsHTTPDaemonRuntime(t, dataDir, srv)

	restore := configureVectorSearchHTTPTest(t, dataDir, true, "")
	defer restore()

	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--mode", "vector",
		"--account", "nobody@nowhere.invalid",
		"hello",
	})

	err := root.Execute()
	require.Error(t, err, "expected daemon scope error")
	assert.ErrorContains(t, err, "no account found")
}

func TestSearchCmd_HybridModeUsesConfiguredRemoteHTTP(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requests := &atomic.Int32{}
	srv := vectorSearchHTTPDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal("hybrid", r.URL.Query().Get("mode"), "mode")
		writeVectorSearchResponse(t, w, "alice@example.com", "", 0)
	})

	restore := configureVectorSearchHTTPTest(t, t.TempDir(), false, srv.URL)
	defer restore()

	done := captureStdout(t)
	root := newTestRootCmd()
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search", "--mode", "hybrid", "--explain", "--json", "lunch"})

	err := root.Execute()
	out := done()
	require.NoError(err, "hybrid remote search command")

	assert.Equal(1, int(requests.Load()), "search endpoint calls")
	assert.Contains(out, `"bm25_score": 1.25`, "bm25_score")
	assert.Contains(out, `"vector_score": 0.9`, "vector_score")
}

func vectorSearchHTTPDaemon(
	t *testing.T,
	searchHandler http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/search", searchHandler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func configureVectorSearchHTTPTest(t *testing.T, dataDir string, local bool, remoteURL string) func() {
	t.Helper()
	savedCfg := cfg
	savedUseLocal := useLocal
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote: config.RemoteConfig{
			URL:           remoteURL,
			AllowInsecure: true,
		},
	}
	if local {
		cfg.Remote.URL = "http://configured-daemonclient.invalid"
	}
	useLocal = local
	resetSearchFlags()
	return func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		resetSearchFlags()
	}
}

func writeVectorSearchResponse(
	t *testing.T,
	w http.ResponseWriter,
	from string,
	scopeLabel string,
	scopeSourceCount int,
) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(map[string]any{
		"returned":           1,
		"pool_saturated":     false,
		"scope_label":        scopeLabel,
		"scope_source_count": scopeSourceCount,
		"generation": map[string]any{
			"id":          7,
			"model":       "fake-model",
			"dimension":   4,
			"fingerprint": "fake:4",
			"state":       "active",
		},
		"results": []map[string]any{{
			"id":      42,
			"subject": "Lunch",
			"from":    from,
			"sent_at": "2024-01-02T03:04:05Z",
			"score": map[string]any{
				"rrf":             0.5,
				"bm25":            1.25,
				"vector":          0.9,
				"subject_boosted": true,
			},
		}},
	})
	require.NoError(t, err, "write vector search response")
}
