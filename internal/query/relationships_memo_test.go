package query

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelationshipsMemoKey(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 30, 0, 0, time.UTC)
	key := relationshipsMemoKey("rev-a", "true", nil, false, base)

	t.Run("same UTC date yields the same key regardless of time of day", func(t *testing.T) {
		later := time.Date(2026, 7, 20, 23, 59, 59, 0, time.UTC)
		assert.Equal(t, key, relationshipsMemoKey("rev-a", "true", nil, false, later))
	})

	t.Run("each input distinguishes keys", func(t *testing.T) {
		assert := assert.New(t)
		assert.NotEqual(key, relationshipsMemoKey("rev-b", "true", nil, false, base))
		assert.NotEqual(key, relationshipsMemoKey("rev-a", "source_id = ?", nil, false, base))
		assert.NotEqual(key, relationshipsMemoKey("rev-a", "true", []any{int64(7)}, false, base))
		assert.NotEqual(key, relationshipsMemoKey("rev-a", "true", nil, true, base))
		assert.NotEqual(key, relationshipsMemoKey("rev-a", "true", nil, false, base.AddDate(0, 0, 1)))
	})

	t.Run("argument values distinguish keys", func(t *testing.T) {
		withSeven := relationshipsMemoKey("rev-a", "source_id = ?", []any{int64(7)}, false, base)
		withEight := relationshipsMemoKey("rev-a", "source_id = ?", []any{int64(8)}, false, base)
		assert.NotEqual(t, withSeven, withEight)
	})
}

func TestRelationshipsMemoRowsCachesPerKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var memo relationshipsMemo
	var computeCalls atomic.Int64
	compute := func(canonicalID int64) func() ([]RelationshipRow, error) {
		return func() ([]RelationshipRow, error) {
			computeCalls.Add(1)
			return []RelationshipRow{{CanonicalID: canonicalID}}, nil
		}
	}

	first, err := memo.rows("key-1", compute(11))
	require.NoError(err)
	second, err := memo.rows("key-1", compute(99))
	require.NoError(err)

	assert.Equal(int64(1), computeCalls.Load(), "second same-key call must be a cache hit")
	require.Len(second, 1)
	assert.Equal(first[0].CanonicalID, second[0].CanonicalID)

	other, err := memo.rows("key-2", compute(22))
	require.NoError(err)
	assert.Equal(int64(2), computeCalls.Load(), "a different key must compute separately")
	require.Len(other, 1)
	assert.Equal(int64(22), other[0].CanonicalID)
}

func TestRelationshipsMemoDoesNotCacheErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var memo relationshipsMemo
	boom := errors.New("query failed")
	_, err := memo.rows("key", func() ([]RelationshipRow, error) { return nil, boom })
	require.ErrorIs(err, boom)

	rows, err := memo.rows("key", func() ([]RelationshipRow, error) {
		return []RelationshipRow{{CanonicalID: 5}}, nil
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(int64(5), rows[0].CanonicalID)
}

func TestRelationshipsMemoEvictsOldestBeyondCapacity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var memo relationshipsMemo
	var computeCalls atomic.Int64
	compute := func() ([]RelationshipRow, error) {
		computeCalls.Add(1)
		return []RelationshipRow{}, nil
	}
	for i := 0; i <= relationshipsMemoMaxEntries; i++ {
		_, err := memo.rows(fmt.Sprintf("key-%d", i), compute)
		require.NoError(err)
	}
	baseline := computeCalls.Load()

	_, err := memo.rows("key-0", compute)
	require.NoError(err)
	assert.Equal(baseline+1, computeCalls.Load(), "oldest key must have been evicted and recomputed")

	_, err = memo.rows(fmt.Sprintf("key-%d", relationshipsMemoMaxEntries), compute)
	require.NoError(err)
	assert.Equal(baseline+1, computeCalls.Load(), "newest key must still be cached")
}

func TestRelationshipsMemoConcurrentCallersShareOneComputation(t *testing.T) {
	var memo relationshipsMemo
	var computeCalls atomic.Int64
	release := make(chan struct{})
	// The always-nil error is imposed by relationshipsMemo.rows' compute
	// signature; this test only exercises the success path.
	compute := func() ([]RelationshipRow, error) { //nolint:unparam
		computeCalls.Add(1)
		<-release
		return []RelationshipRow{{CanonicalID: 1}}, nil
	}

	const callers = 8
	var started, done sync.WaitGroup
	started.Add(callers)
	done.Add(callers)
	for range callers {
		go func() {
			started.Done()
			rows, err := memo.rows("shared", compute)
			assert.NoError(t, err)
			assert.Len(t, rows, 1)
			done.Done()
		}()
	}
	started.Wait()
	close(release)
	done.Wait()

	assert.Equal(t, int64(1), computeCalls.Load(), "concurrent identical requests must share one computation")
}
