//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
			require.NoError(t, writeHybridResultsTable(&buf, []daemonclient.CLIHybridSearchResult{tt.result}, false, false))
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
	assert := assert.New(t)
	rrf, bm25, vec := 0.5, 1.25, 0.75
	result := daemonclient.CLIHybridSearchResult{
		ID: 204, SentAt: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		FromEmail: "frank@example.com", Subject: "Album", SubjectBoosted: true,
		RRFScore: &rrf, BM25Score: &bm25, VectorScore: &vec,
	}
	var buf bytes.Buffer
	require.NoError(t, writeHybridResultsTable(&buf, []daemonclient.CLIHybridSearchResult{result}, true, false))
	assert.Contains(buf.String(), "ID")
	assert.Contains(buf.String(), "RRF")
	assert.Contains(buf.String(), "BM25")
	assert.Contains(buf.String(), "VEC")
	assert.Contains(buf.String(), "Album *")
	assert.Contains(buf.String(), "0.5000")
	assert.Contains(buf.String(), "1.2500")
	assert.Contains(buf.String(), "0.7500")
}

func TestSearchHybridFullTextInPipe(t *testing.T) {
	from := strings.Repeat("sender", 10) + "@example.com"
	text := strings.Repeat("complete text ", 10) + "final words"
	for _, explain := range []bool{false, true} {
		t.Run(strconv.FormatBool(explain), func(t *testing.T) {
			assert := assert.New(t)
			done := captureStdout(t)
			err := outputHybridResultsTable(&daemonclient.CLIHybridSearch{Results: []daemonclient.CLIHybridSearchResult{
				{ID: 1, FromEmail: from, Subject: "\x1b[31m" + text, SubjectBoosted: true},
				{ID: 2, Message: query.MessageSummary{FromName: from, Snippet: text + "\n"}},
			}}, explain)
			output := done()
			require.NoError(t, err)
			assert.Contains(output, from)
			assert.Contains(output, text+" *")
			assert.NotContains(output, "\x1b")
			assert.Equal(2, strings.Count(output, text))
		})
	}
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

	testCtx, restore := configureVectorSearchHTTPTest(t, dataDir, true, "")
	defer restore()

	done := captureStdout(t)
	root := newTestRootCmd()
	root.SetContext(testCtx)
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
	assert.Contains(out, `"took_ms": 12`, "total timing")
	assert.Contains(out, `"query_embedding_ms": 2`, "query embedding timing")
	assert.Contains(out, `"retrieval_ms": 7`, "retrieval timing")
	assert.Contains(out, `"accelerator": "vec1_ivf_opq"`, "accelerator path")
	assert.Contains(out, `"hydration_ms": 3`, "hydration timing")
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

	testCtx, restore := configureVectorSearchHTTPTest(t, dataDir, true, "")
	defer restore()

	doneOut := captureStdout(t)
	doneErr := captureStderr(t)
	root := newTestRootCmd()
	root.SetContext(testCtx)
	root.AddCommand(searchCmd)
	root.SetArgs([]string{
		"search", "--mode", "vector",
		"--explain",
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
	assert.Contains(out, "Timings: total=12ms query_embedding=2ms retrieval=7ms hydration=3ms", "timings")
	assert.Contains(out, "Accelerator: vec1_ivf_opq", "accelerator path")
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

	testCtx, restore := configureVectorSearchHTTPTest(t, dataDir, true, "")
	defer restore()

	root := newTestRootCmd()
	root.SetContext(testCtx)
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

	testCtx, restore := configureVectorSearchHTTPTest(t, t.TempDir(), false, srv.URL)
	defer restore()

	done := captureStdout(t)
	root := newTestRootCmd()
	root.SetContext(testCtx)
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search", "--mode", "hybrid", "--explain", "--json", "lunch"})

	err := root.Execute()
	out := done()
	require.NoError(err, "hybrid remote search command")

	assert.Equal(1, int(requests.Load()), "search endpoint calls")
	assert.Contains(out, `"bm25_score": 1.25`, "bm25_score")
	assert.Contains(out, `"vector_score": 0.9`, "vector_score")
	assert.Contains(out, `"accelerator": "vec1_ivf_opq"`, "accelerator path")
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

func configureVectorSearchHTTPTest(t *testing.T, dataDir string, local bool, remoteURL string) (context.Context, func()) {
	t.Helper()
	cfg := testConfigValue()
	useLocal := false
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
	testCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	invocationFromContext(testCtx).options.useLocal = local
	if local {
		cfg.Remote.URL = "http://configured-daemonclient.invalid"
	}
	useLocal = local
	resetSearchFlags()
	return testCtx, func() {
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
		"returned":       1,
		"pool_saturated": false,
		"accelerator":    "vec1_ivf_opq",
		"took_ms":        12,
		"timings": map[string]any{
			"query_embedding_ms": 2,
			"retrieval_ms":       7,
			"hydration_ms":       3,
		},
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

func writeRerankedSearchResponse(t *testing.T, w http.ResponseWriter, rerank map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	score := map[string]any{"rrf": 0.5, "vector": 0.9}
	if rerank["applied"] == true {
		score["rerank"] = 0.93
	}
	result := map[string]any{
		"id": 42, "subject": "Parcel", "from": "alice@example.com", "sent_at": "2024-01-02T03:04:05Z",
		"score": score,
	}
	err := json.NewEncoder(w).Encode(map[string]any{
		"returned": 1, "pool_saturated": false, "took_ms": 420,
		"timings": map[string]any{"query_embedding_ms": 2, "retrieval_ms": 7, "hydration_ms": 3, "rerank_ms": 408},
		"rerank":  rerank,
		"generation": map[string]any{
			"id": 7, "model": "fake-model", "dimension": 4, "fingerprint": "fake:4", "state": "active",
		},
		"results": []map[string]any{result},
	})
	require.NoError(t, err, "write reranked search response")
}

func TestSearchCmd_RerankFlagForwardsAndReportsStage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	srv := vectorSearchHTTPDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("true", r.URL.Query().Get("rerank"), "rerank query")
		writeRerankedSearchResponse(t, w, map[string]any{
			"applied": true, "model": "cohere/rerank-4-pro", "candidates": 50,
		})
	})
	writeStatsHTTPDaemonRuntime(t, dataDir, srv)
	testCtx, restore := configureVectorSearchHTTPTest(t, dataDir, true, "")
	defer restore()

	done := captureStdout(t)
	root := newTestRootCmd()
	root.SetContext(testCtx)
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search", "--mode", "hybrid", "--rerank", "--explain", "when does the parcel arrive"})
	err := root.Execute()
	out := done()
	require.NoError(err)

	assert.Contains(out, "RERANK", "explain table gains a rerank column")
	assert.Contains(out, "0.9300", "rerank score")
	assert.Contains(out, "Reranked the top 50 with cohere/rerank-4-pro")
	assert.Contains(out, "rerank=408ms", "rerank timing")
}

func TestSearchCmd_RerankFallbackWarnsOnStderrAndKeepsJSONClean(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	srv := vectorSearchHTTPDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		writeRerankedSearchResponse(t, w, map[string]any{
			"applied": false, "model": "cohere/rerank-4-pro", "candidates": 0, "fallback": "timeout",
		})
	})
	writeStatsHTTPDaemonRuntime(t, dataDir, srv)
	testCtx, restore := configureVectorSearchHTTPTest(t, dataDir, true, "")
	defer restore()

	doneOut := captureStdout(t)
	doneErr := captureStderr(t)
	root := newTestRootCmd()
	root.SetContext(testCtx)
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search", "--mode", "hybrid", "--rerank", "--json", "when does the parcel arrive"})
	err := root.Execute()
	out := doneOut()
	errOut := doneErr()
	require.NoError(err)

	var decoded map[string]any
	require.NoError(json.Unmarshal([]byte(out), &decoded), "stdout stays valid JSON")
	assert.Equal(map[string]any{
		"applied": false, "model": "cohere/rerank-4-pro", "candidates": float64(0), "fallback": "timeout",
	}, decoded["rerank"])
	assert.Contains(errOut, "Reranking failed (timeout); showing hybrid order")
}

func TestSearchCmd_RerankOmittedLeavesDaemonDefault(t *testing.T) {
	dataDir := t.TempDir()
	srv := vectorSearchHTTPDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		_, present := r.URL.Query()["rerank"]
		assert.False(t, present, "an omitted --rerank sends no rerank parameter")
		writeVectorSearchResponse(t, w, "bob@example.com", "", 0)
	})
	writeStatsHTTPDaemonRuntime(t, dataDir, srv)
	testCtx, restore := configureVectorSearchHTTPTest(t, dataDir, true, "")
	defer restore()

	done := captureStdout(t)
	root := newTestRootCmd()
	root.SetContext(testCtx)
	root.AddCommand(searchCmd)
	root.SetArgs([]string{"search", "--mode", "vector", "lunch"})
	err := root.Execute()
	_ = done()
	require.NoError(t, err)
}
