package store_test

import (
	"fmt"
	"os"
	"sync"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSourceLifetimePostgreSQLNativeIdentityAndConvergence(t *testing.T) {
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("requires the isolated PostgreSQL test backend")
	}
	require := Require.New(t)
	assert := Assert.New(t)
	first := testutil.NewTestStore(t)
	second, err := store.OpenForTest(store.DBPathForTest(first))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(second.Close()) })
	var cycles bool
	require.NoError(first.DB().QueryRow(`SELECT seqcycle FROM pg_sequence
		WHERE seqrelid=pg_get_serial_sequence('sources','id')::regclass`).Scan(&cycles))
	assert.False(cycles, "source allocation must retain the native noncycling identity")
	var counterRows int
	require.NoError(first.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata
		WHERE key='source_id_high_water'`).Scan(&counterRows))
	assert.Zero(counterRows, "PostgreSQL does not use the SQLite metadata counter")
	type result struct {
		source *store.Source
		err    error
	}
	for _, same := range []bool{true, false} {
		start := make(chan struct{})
		results := make(chan result, 2)
		var workers sync.WaitGroup
		for i, st := range []*store.Store{first, second} {
			workers.Go(func() {
				<-start
				identifier := "converged@example.test"
				if !same {
					identifier = fmt.Sprintf("pg-worker%d@example.test", i)
				}
				source, err := st.GetOrCreateSourceContext(t.Context(), "gmail", identifier)
				results <- result{source, err}
			})
		}
		close(start)
		workers.Wait()
		close(results)
		ids := make(map[int64]bool)
		for result := range results {
			require.NoError(result.err)
			require.NotNil(result.source)
			ids[result.source.ID] = true
		}
		if same {
			assert.Len(ids, 1)
		} else {
			assert.Len(ids, 2)
		}
	}
	before, err := first.GetOrCreateSource("gmail", "pg-recreated@example.test")
	require.NoError(err)
	require.NoError(first.RemoveSource(before.ID))
	after, err := second.GetOrCreateSource("gmail", before.Identifier)
	require.NoError(err)
	assert.Greater(after.ID, before.ID)
}
