package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func TestMetadataSearchDuckDBLazyStats(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newParquetEngine(t)
	q := search.Parse("after:2024-01-01")
	without, err := engine.SearchFastWithStats(t.Context(), q, "after:2024-01-01", MessageFilter{}, ViewNoStats, 2, 0)
	require.NoError(err)
	require.Len(without.Messages, 2)
	assert.Nil(without.Stats)
	assert.Nil(engine.searchCacheStats, "no aggregates should be computed")
	with, err := engine.SearchFastWithStats(t.Context(), q, "after:2024-01-01", MessageFilter{}, ViewSenders, 2, 0)
	require.NoError(err)
	require.NotNil(with.Stats)
	assert.Equal(without.Messages, with.Messages)
	assert.Equal(without.TotalCount, with.TotalCount)
	assert.Equal(with.TotalCount, with.Stats.MessageCount)
	without, err = engine.SearchFastWithStats(t.Context(), q, "after:2024-01-01", MessageFilter{}, ViewNoStats, 2, 0)
	require.NoError(err)
	assert.Nil(without.Stats, "cached stats must not leak into stats-free responses")
	countOnly, err := engine.SearchFastWithStats(t.Context(), q, "after:2024-01-01", MessageFilter{}, ViewNoStats, 0, 0)
	require.NoError(err)
	assert.Empty(countOnly.Messages)
	assert.Equal(with.TotalCount, countOnly.TotalCount)
}
