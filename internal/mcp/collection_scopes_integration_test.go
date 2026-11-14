package mcp

import (
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
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
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	first, firstID, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "first@example.test", "first", "first alpine fixture", "alpine body")
	require.NoError(err)
	second, secondID, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "second@example.test", "second", "other alpine fixture", "alpine body")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET sent_at = ? WHERE id IN (?, ?)"), "2025-01-01T00:00:00Z", firstID, secondID)
	require.NoError(err)
	_, err = st.CreateCollection("selected", "", []int64{first.ID})
	require.NoError(err)
	_, err = st.CreateCollection("empty", "", []int64{second.ID})
	require.NoError(err)
	err = st.RemoveSourcesFromCollection("empty", []int64{second.ID})
	require.NoError(err)
	_, err = st.CreateCollection("both", "", []int64{first.ID, second.ID})
	require.NoError(err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner"}}
	cfg.Data.DataDir = t.TempDir()
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	session := task5ConnectClient(t, ServeOptions{Engine: daemonclient.NewEngineAdapter(client), CollectionScopesAvailable: true}, false)

	listed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolListMessageCollections, Arguments: map[string]any{}})
	require.NoError(err)
	require.False(listed.IsError)
	content, ok := listed.StructuredContent.(map[string]any)
	require.True(ok)
	collections, ok := content["collections"].([]any)
	require.True(ok)
	assert.ElementsMatch([]any{
		map[string]any{"name": "selected", "source_ids": []any{float64(first.ID)}},
		map[string]any{"name": "empty", "source_ids": []any{}},
		map[string]any{"name": "both", "source_ids": []any{float64(first.ID), float64(second.ID)}},
	}, collections)
	got, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSearchMessageBodies, Arguments: map[string]any{"collection": "both", "query": "alpine"}})
	require.NoError(err)
	require.True(got.IsError)
	require.NotEmpty(got.Content)
	errorText, ok := got.Content[0].(*sdkmcp.TextContent)
	require.True(ok)
	assert.Contains(errorText.Text, "collection contains multiple sources")
	for _, tc := range []struct {
		id         int64
		collection string
		denied     bool
	}{
		{firstID, "selected", false}, {secondID, "selected", true}, {firstID, "empty", true},
	} {
		got, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessage, Arguments: map[string]any{"id": tc.id, "collection": tc.collection}})
		require.NoError(callErr)
		assert.Equal(tc.denied, got.IsError)
	}
	for _, collection := range []string{"empty", "selected"} {
		got, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSearchMessageBodies, Arguments: map[string]any{"query": "alpine", "collection": collection}})
		require.NoError(callErr)
		require.False(got.IsError)
		if collection == "empty" {
			assert.NotContains(fmt.Sprint(got.StructuredContent), "alpine fixture")
		} else {
			assert.Contains(fmt.Sprint(got.StructuredContent), "alpine fixture")
			assert.NotContains(fmt.Sprint(got.StructuredContent), "other alpine fixture")
		}
	}
	for _, tc := range []struct {
		collection string
		count      float64
	}{{"empty", 0}, {"selected", 1}, {"both", 2}} {
		got, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolAggregate, Arguments: map[string]any{"group_by": "time", "collection": tc.collection}})
		require.NoError(callErr)
		require.False(got.IsError)
		content, ok := got.StructuredContent.(map[string]any)
		require.True(ok)
		rows, ok := content["data"].([]any)
		require.True(ok)
		if tc.count == 0 {
			assert.Empty(rows)
		} else {
			require.Len(rows, 1)
			row, ok := rows[0].(map[string]any)
			require.True(ok)
			count, ok := row["Count"].(float64)
			require.True(ok)
			assert.InDelta(tc.count, count, 0)
		}
	}
	for _, tool := range []string{ToolSearchMetadata, ToolListMessages} {
		for _, tc := range []struct {
			collection            string
			wantFirst, wantSecond bool
		}{{"empty", false, false}, {"both", true, true}, {"selected", true, false}} {
			args := map[string]any{"collection": tc.collection}
			if tool == ToolSearchMetadata {
				args["query"] = "alpine"
			}
			got, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: args})
			require.NoError(callErr)
			require.False(got.IsError)
			content := fmt.Sprint(got.StructuredContent)
			assert.Equal(tc.wantFirst, strings.Contains(content, "first alpine fixture"))
			assert.Equal(tc.wantSecond, strings.Contains(content, "other alpine fixture"))
		}
	}
	require.NoError(st.AddSourcesToCollection("selected", []int64{second.ID}))
	updated, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolListMessages, Arguments: map[string]any{"collection": "selected"}})
	require.NoError(err)
	require.False(updated.IsError)
	assert.Contains(fmt.Sprint(updated.StructuredContent), "other alpine fixture")
	hidden, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolListMessages, Arguments: map[string]any{"collection": "All"}})
	require.NoError(err)
	require.True(hidden.IsError)
	require.NotEmpty(hidden.Content)
	hiddenText, ok := hidden.Content[0].(*sdkmcp.TextContent)
	require.True(ok)
	assert.Contains(hiddenText.Text, "collection not found")
	bad, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolListMessageCollections, Arguments: map[string]any{"unexpected": true}})
	require.NoError(err)
	require.True(bad.IsError)
	require.NotEmpty(bad.Content)
	badText, ok := bad.Content[0].(*sdkmcp.TextContent)
	require.True(ok)
	assert.Contains(badText.Text, `validating "arguments"`)
}
