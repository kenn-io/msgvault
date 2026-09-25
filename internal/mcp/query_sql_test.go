package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type sqlToolEngine struct {
	*querytest.MockEngine

	fresh bool
}

func (e *sqlToolEngine) QueryArchiveSQL(_ context.Context, sql string, fresh bool) (*query.QueryResult, *daemonclient.CacheBuildAccepted, error) {
	e.fresh = fresh
	if fresh {
		return nil, &daemonclient.CacheBuildAccepted{Status: "queued", JobID: "job-1"}, nil
	}
	return &query.QueryResult{Columns: []string{"value"}, Rows: [][]any{{1}}, RowCount: 1}, nil, nil
}

func TestQuerySQLToolReturnsRowsOrAcceptedBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	engine := &sqlToolEngine{MockEngine: &querytest.MockEngine{}}
	options := ServeOptions{Engine: engine}
	listed := toolsByName(t, rawListTools(t, options, false))
	require.Contains(listed, ToolQuerySQL)
	assert.Equal(true, toolReadOnlyHint(t, listed[ToolQuerySQL]))

	result := rawCallTool(t, options, ToolQuerySQL, map[string]any{"sql": "SELECT 1"})
	assert.NotEqual(true, result["isError"])
	structured, ok := result["structuredContent"].(map[string]any)
	require.True(ok)
	assert.InDelta(float64(1), structured["row_count"], 0)
	assert.False(engine.fresh)

	result = rawCallTool(t, options, ToolQuerySQL, map[string]any{"sql": "SELECT 1", "fresh": true})
	assert.NotEqual(true, result["isError"])
	structured, ok = result["structuredContent"].(map[string]any)
	require.True(ok)
	assert.Equal("job-1", structured["job_id"])
	assert.True(engine.fresh)

	result = rawCallTool(t, options, ToolQuerySQL, map[string]any{"sql": "DELETE FROM messages"})
	assert.Equal(true, result["isError"])
}

func TestQuerySQLToolRequiresArchiveCapability(t *testing.T) {
	engine, err := query.NewDuckDBEngine("", "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	listed := toolsByName(t, rawListTools(t, ServeOptions{Engine: engine}, false))
	_, hasQuerySQL := listed[ToolQuerySQL]
	assert.False(t, hasQuerySQL)
}

func TestQuerySQLToolConfinesDaemonOwnerCredentialsToArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	analyticsDir := t.TempDir()
	fixtureDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(fixtureDB.Close()) })
	// View schemas are exercised by query's engine tests. This transport fixture
	// needs committed Parquet data to query through the native archive boundary.
	for _, dataset := range query.RequiredParquetDirs {
		dir := filepath.Join(analyticsDir, dataset)
		require.NoError(os.MkdirAll(dir, 0o700))
		path := strings.ReplaceAll(filepath.ToSlash(filepath.Join(dir, "rows.parquet")), "'", "''")
		_, err := fixtureDB.Exec("COPY (SELECT 'synthetic archive row' AS subject) TO '" + path + "' (FORMAT PARQUET)")
		require.NoError(err)
	}
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	require.NoError(err)
	state, err := json.Marshal(query.CacheSyncState{
		LastSyncAt: time.Now().UTC(), PublishedAt: time.Now().UTC(),
		SchemaVersion: query.CacheSchemaVersion, DatasetFingerprint: fingerprint,
	})
	require.NoError(err)
	require.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), state, 0o600))
	options := query.DuckDBOptions{DisableLegacyAnalyticalViews: true}
	owner, err := query.NewDuckDBEngine(analyticsDir, "", nil, options)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(owner.Close()) })
	archive, err := query.NewArchiveDuckDBEngine(analyticsDir, options)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(archive.Close()) })
	daemon := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}},
		Engine: owner, Logger: slog.New(slog.DiscardHandler),
		ArchiveSQLQueryRunner: func(ctx context.Context, statement string, _ bool) (*query.QueryResult, *api.CacheBuildAccepted, error) {
			result, err := archive.QuerySQL(ctx, statement)
			return result, nil, err
		},
	})
	daemonHTTP := httptest.NewServer(daemon.Router())
	t.Cleanup(daemonHTTP.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemonHTTP.URL, APIKey: "synthetic-owner-key", AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(client.Close()) })
	adapter := daemonclient.NewEngineAdapter(client)
	mcpHandler := newMCPHTTPServer(ServeOptions{Engine: adapter}, HTTPOptions{APIKey: "synthetic-mcp-key"}).Handler
	archivePath := strings.ReplaceAll(filepath.ToSlash(filepath.Join(analyticsDir, "messages", "rows.parquet")), "'", "''")
	result := callSQLToolHTTP(t, mcpHandler, "SELECT subject FROM read_parquet('"+archivePath+"')", "synthetic-mcp-key")
	require.Empty(result.Error)
	require.NotEqual(true, result.Result["isError"])
	structured, ok := result.Result["structuredContent"].(map[string]any)
	require.True(ok)
	assert.Equal([]any{[]any{"synthetic archive row"}}, structured["rows"])

	outsidePath := filepath.Join(t.TempDir(), "outside-archive.txt")
	require.NoError(os.WriteFile(outsidePath, []byte("synthetic outside row"), 0o600))
	outsideSQLPath := strings.ReplaceAll(filepath.ToSlash(outsidePath), "'", "''")
	for _, reader := range []string{"read_text", "read_blob"} {
		result := callSQLToolHTTP(t, mcpHandler, fmt.Sprintf("SELECT content FROM %s('%s')", reader, outsideSQLPath), "synthetic-mcp-key")
		assert.NotEmpty(result.Error, reader)
		assert.NotContains(fmt.Sprint(result), "synthetic outside row")
	}
	ownerResult, err := client.RunSQLQuery(t.Context(), "SELECT content FROM read_text('"+outsideSQLPath+"')")
	require.NoError(err)
	assert.Equal([][]any{{"synthetic outside row"}}, ownerResult.Rows)

	mcpOnlyRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, daemonHTTP.URL+"/api/v1/query", strings.NewReader(`{"sql":"SELECT 1"}`))
	require.NoError(err)
	mcpOnlyRequest.Header.Set("Authorization", "Bearer synthetic-mcp-key")
	response, err := daemonHTTP.Client().Do(mcpOnlyRequest)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(response.Body.Close()) })
	assert.Equal(http.StatusUnauthorized, response.StatusCode)
}

func TestQuerySQLToolDoesNotUseOwnerOnlyDaemon(t *testing.T) {
	// This server represents an older daemon with only the owner SQL endpoint.
	// Exercise the actual client and MCP handler to prove a missing restricted
	// endpoint cannot fall back to the owner's broader query capability.
	var ownerCalls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		ownerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"columns":["content"],"rows":[["owner-only"]],"row_count":1}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(daemon.Close)
	engine, err := daemonclient.NewEngine(daemonclient.Config{URL: daemon.URL, APIKey: "synthetic-owner-key", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	handler := newMCPHTTPServer(ServeOptions{Engine: engine}, HTTPOptions{}).Handler
	result := callSQLToolHTTP(t, handler, "SELECT 1", "")
	assert.NotEmpty(t, result.Error)
	assert.Zero(t, ownerCalls.Load())
}

func callSQLToolHTTP(t *testing.T, handler http.Handler, statement, apiKey string) rawRPCResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
			"name": ToolQuerySQL, "arguments": map[string]any{"sql": statement}, "_meta": modernRequestMeta(),
		},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", modernProtocolVersion)
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", ToolQuerySQL)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result rawRPCResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	return result
}
