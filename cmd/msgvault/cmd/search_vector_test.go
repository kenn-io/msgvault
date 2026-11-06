//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
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
	assert := assert.New(t)
	rrf, bm25, vec := 0.5, 1.25, 0.75
	result := daemonclient.CLIHybridSearchResult{
		ID: 204, SentAt: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		FromEmail: "frank@example.com", Subject: "Album", SubjectBoosted: true,
		RRFScore: &rrf, BM25Score: &bm25, VectorScore: &vec,
	}
	var buf bytes.Buffer
	require.NoError(t, writeHybridResultsTable(&buf, []daemonclient.CLIHybridSearchResult{result}, true))
	assert.Contains(buf.String(), "ID")
	assert.Contains(buf.String(), "RRF")
	assert.Contains(buf.String(), "BM25")
	assert.Contains(buf.String(), "VEC")
	assert.Contains(buf.String(), "Album *")
	assert.Contains(buf.String(), "0.5000")
	assert.Contains(buf.String(), "1.2500")
	assert.Contains(buf.String(), "0.7500")
}

func TestOutputHybridResultsJSONHiddenTextPresent(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	done := captureStdout(t)
	err := outputHybridResultsJSON(&daemonclient.CLIHybridSearch{Results: []daemonclient.CLIHybridSearchResult{
		{ID: 1, Message: query.MessageSummary{HiddenTextPresent: true}},
		{ID: 2, Message: query.MessageSummary{HiddenTextPresent: false}},
	}}, false)
	out := done()
	require.NoError(err)
	var result struct {
		Results []map[string]any `json:"results"`
	}
	require.NoError(json.Unmarshal([]byte(out), &result))
	require.Len(result.Results, 2)
	assert.Equal(true, result.Results[0]["hidden_text_present"])
	assert.Equal(false, result.Results[1]["hidden_text_present"])
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
		writeVectorSearchResponse(t, w, 1, "", 0)
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
		writeVectorSearchResponse(t, w, 1, "Important", 2)
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
		writeVectorSearchResponse(t, w, 1, "", 0)
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

func TestSearchCmd_HintsUseConfiguredRemoteHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, mode           string
		hasMore, pool        bool
		cap, offset, results int
		codes, messages      []string
	}{
		{name: "complete", results: 1, mode: "vector"},
		{name: "more", results: 50, mode: "vector", hasMore: true, codes: []string{"more_available"}, messages: []string{"more messages match; raise --limit"}},
		{name: "saturated_more", results: 50, mode: "hybrid", hasMore: true, pool: true, codes: []string{"more_available"}, messages: []string{"more messages match; raise --limit"}},
		{name: "ranking_cap", results: 50, mode: "vector", hasMore: true, cap: 50, codes: []string{"ranking_limit_reached"}, messages: []string{"vector.search.max_page_size_hybrid above 50"}},
		{name: "pool_only", results: 1, mode: "hybrid", pool: true, codes: []string{"candidate_pool_saturated"}, messages: []string{"vector.search.k_per_signal"}},
		{name: "dual_cap_pool", results: 50, mode: "hybrid", pool: true, cap: 50, codes: []string{"ranking_limit_reached", "candidate_pool_saturated"}, messages: []string{"vector.search.max_page_size_hybrid above 50", "vector.search.k_per_signal"}},
		{name: "fts_complete", mode: "fts"},
		{name: "fts_later_page", mode: "fts", hasMore: true, offset: 50, messages: []string{"--offset 100"}},
	} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", tc.name, asJSON), func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				requests := &atomic.Int32{}
				srv := vectorSearchHTTPDaemon(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if tc.mode == "fts" {
						assert.Equal("50", r.URL.Query().Get("limit"))
						if tc.offset > 0 {
							assert.Equal("50", r.URL.Query().Get("offset"))
						}
						rows := make([]map[string]any, 50)
						for i := range rows {
							rows[i] = map[string]any{"id": i + 1, "subject": "Lunch"}
						}
						assert.NoError(json.NewEncoder(w).Encode(map[string]any{"results": rows, "has_more": tc.hasMore}))
						return
					}
					assert.Equal(tc.mode, r.URL.Query().Get("mode"))
					writeVectorSearchResponse(t, w, tc.results, "", 0, map[string]any{"mode": tc.mode, "has_more": tc.hasMore, "pool_saturated": tc.pool, "ranking_limit_reached": tc.cap})
				})
				ctx, restore := configureVectorSearchHTTPTest(t, t.TempDir(), false, srv.URL)
				defer restore()
				root := newTestRootCmd()
				root.SetContext(ctx)
				root.AddCommand(searchCmd)
				args := []string{"search", "--mode", tc.mode, "lunch"}
				if tc.mode == "fts" {
					args = append(args, "--offset", strconv.Itoa(tc.offset))
				}
				if asJSON {
					args = append(args, "--json")
				}
				root.SetArgs(args)
				var stderr bytes.Buffer
				root.SetErr(&stderr)
				done := captureStdout(t)
				err := root.Execute()
				out := done()
				require.NoError(err)
				assert.Equal(int32(1), requests.Load())
				if asJSON && tc.mode != "fts" {
					var page struct {
						Hints []struct {
							Code    string `json:"code"`
							Message string `json:"message"`
						} `json:"hints"`
					}
					require.NoError(json.Unmarshal([]byte(out), &page))
					require.Len(page.Hints, len(tc.codes))
					for i, code := range tc.codes {
						assert.Equal(code, page.Hints[i].Code)
						assert.Contains(page.Hints[i].Message, tc.messages[i])
					}
					assert.Empty(stderr.String())
				} else {
					assert.Equal(len(tc.messages), strings.Count(stderr.String(), "Hint:"))
					for _, message := range tc.messages {
						assert.Contains(stderr.String(), message)
					}
					if asJSON {
						var rows []map[string]any
						require.NoError(json.Unmarshal([]byte(out), &rows))
						assert.Len(rows, 50)
					}
				}
			})
		}
	}
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
	mux.HandleFunc("/api/v1/cli/search", searchHandler)
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
	resultCount int,
	scopeLabel string,
	scopeSourceCount int,
	metadata ...map[string]any,
) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	rows := make([]map[string]any, resultCount)
	for i := range rows {
		rows[i] = map[string]any{
			"id":      42 + i,
			"subject": "Lunch",
			"from":    "alice@example.com",
			"sent_at": "2024-01-02T03:04:05Z",
			"score": map[string]any{
				"rrf":             0.5,
				"bm25":            1.25,
				"vector":          0.9,
				"subject_boosted": true,
			},
		}
	}
	response := map[string]any{
		"returned":       resultCount,
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
		"results": rows,
	}
	for _, values := range metadata {
		maps.Copy(response, values)
	}
	err := json.NewEncoder(w).Encode(response)
	require.NoError(t, err, "write vector search response")
}
