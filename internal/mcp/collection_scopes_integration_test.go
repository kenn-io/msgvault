package mcp

import (
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCollectionsThroughDaemonAndSDK(t *testing.T) {
	st := testutil.NewTestStore(t)
	first, firstID, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "first@example.test", "first", "alpine fixture", "alpine body")
	require.NoError(t, err)
	second, secondID, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "second@example.test", "second", "other alpine fixture", "alpine body")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET sent_at = ? WHERE id IN (?, ?)"), "2025-01-01T00:00:00Z", firstID, secondID)
	require.NoError(t, err)
	_, err = st.CreateCollection("selected", "", []int64{first.ID})
	require.NoError(t, err)
	_, err = st.CreateCollection("empty", "", []int64{second.ID})
	require.NoError(t, err)
	err = st.RemoveSourcesFromCollection("empty", []int64{second.ID})
	require.NoError(t, err)
	_, err = st.CreateCollection("both", "", []int64{first.ID, second.ID})
	require.NoError(t, err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner"}}
	cfg.Data.DataDir = t.TempDir()
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	session := task5ConnectClient(t, ServeOptions{Engine: daemonclient.NewEngineAdapter(client), CollectionScopesAvailable: true}, false)

	listed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolListMessageCollections, Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, listed.IsError)
	content, ok := listed.StructuredContent.(map[string]any)
	require.True(t, ok)
	collections, ok := content["collections"].([]any)
	require.True(t, ok)
	assert.ElementsMatch(t, []any{
		map[string]any{"name": "selected", "source_ids": []any{float64(first.ID)}},
		map[string]any{"name": "empty", "source_ids": []any{}},
		map[string]any{"name": "both", "source_ids": []any{float64(first.ID), float64(second.ID)}},
	}, collections)
	got, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSearchMessageBodies, Arguments: map[string]any{"collection": "both", "query": "alpine"}})
	require.NoError(t, err)
	require.True(t, got.IsError)
	require.NotEmpty(t, got.Content)
	errorText, ok := got.Content[0].(*sdkmcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, errorText.Text, "collection contains multiple sources")
	for _, tc := range []struct {
		id         int64
		collection string
		denied     bool
	}{
		{firstID, "selected", false}, {secondID, "selected", true}, {firstID, "empty", true},
	} {
		got, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessage, Arguments: map[string]any{"id": tc.id, "collection": tc.collection}})
		require.NoError(t, callErr)
		assert.Equal(t, tc.denied, got.IsError)
	}
	for _, collection := range []string{"empty", "selected"} {
		got, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSearchMessageBodies, Arguments: map[string]any{"query": "alpine", "collection": collection}})
		require.NoError(t, callErr)
		require.False(t, got.IsError)
		if collection == "empty" {
			assert.NotContains(t, fmt.Sprint(got.StructuredContent), "alpine fixture")
		} else {
			assert.Contains(t, fmt.Sprint(got.StructuredContent), "alpine fixture")
			assert.NotContains(t, fmt.Sprint(got.StructuredContent), "other alpine fixture")
		}
	}
	for _, tc := range []struct {
		collection string
		count      float64
	}{{"empty", 0}, {"selected", 1}, {"both", 2}} {
		got, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolAggregate, Arguments: map[string]any{"group_by": "time", "collection": tc.collection}})
		require.NoError(t, callErr)
		require.False(t, got.IsError)
		content, ok := got.StructuredContent.(map[string]any)
		require.True(t, ok)
		rows, ok := content["data"].([]any)
		require.True(t, ok)
		if tc.count == 0 {
			assert.Empty(t, rows)
		} else {
			require.Len(t, rows, 1)
			row, ok := rows[0].(map[string]any)
			require.True(t, ok)
			count, ok := row["Count"].(float64)
			require.True(t, ok)
			assert.InDelta(t, tc.count, count, 0)
		}
	}
	bad, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolListMessageCollections, Arguments: map[string]any{"unexpected": true}})
	require.NoError(t, err)
	require.True(t, bad.IsError)
	require.NotEmpty(t, bad.Content)
	badText, ok := bad.Content[0].(*sdkmcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, badText.Text, `validating "arguments"`)
}
