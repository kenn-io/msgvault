package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type sqlToolEngine struct {
	*querytest.MockEngine

	fresh bool
}

func (e *sqlToolEngine) QuerySQLWithFresh(_ context.Context, sql string, fresh bool) (*query.QueryResult, *daemonclient.CacheBuildAccepted, error) {
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
