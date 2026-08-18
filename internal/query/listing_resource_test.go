package query

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func constrainedListingEngine(t *testing.T) *DuckDBEngine {
	t.Helper()
	engine := buildBenchData(t)
	_, err := engine.db.Exec("SET memory_limit='160MB'")
	require.NoError(t, err)
	return engine
}

func TestExploreFastPathFitsConstrainedMemory(t *testing.T) {
	engine := constrainedListingEngine(t)
	result, err := engine.Explore(context.Background(), ExploreRequest{
		Page: PageSpec{Limit: 50},
	})
	require.NoError(t, err)
	assert.Len(t, result.Rows, 50)
	assert.Equal(t, int64(104), result.TotalCount)
}

func TestExploreFastPathCountsBeyondEndWithinConstrainedMemory(t *testing.T) {
	engine := constrainedListingEngine(t)
	result, err := engine.Explore(context.Background(), ExploreRequest{
		Page: PageSpec{Limit: 50, Offset: 500},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Rows)
	assert.Equal(t, int64(104), result.TotalCount)
}

func TestFileSearchFastPathFitsConstrainedMemory(t *testing.T) {
	engine := constrainedListingEngine(t)
	result, err := engine.SearchFiles(context.Background(), FileSearchRequest{
		Page: PageSpec{Limit: 500},
	})
	require.NoError(t, err)
	assert.Len(t, result.Files, 500)
	assert.Equal(t, int64(20_000), result.TotalCount)
}
