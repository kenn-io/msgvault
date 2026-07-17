package query

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQuerySQLHonorsContextCancellation proves that a long-running DuckDB
// query aborts promptly when its context is cancelled, instead of running to
// completion and pegging every core (the F2 runaway-query incident). The
// go-duckdb driver interrupts the in-flight query on ctx cancellation; this
// test guards that the engine passes the request context all the way down.
func TestQuerySQLHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	engine, err := NewDuckDBEngine("", "", nil)
	require.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after the query starts. Uncancelled, the cross join
	// below (1e6 x 1e6 rows) would run for many minutes.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	const slowSQL = `
		SELECT COUNT(*)
		FROM range(1000000) a, range(1000000) b
		WHERE (a.range * b.range) % 7 = 0`

	start := time.Now()
	_, err = engine.QuerySQL(ctx, slowSQL)
	elapsed := time.Since(start)

	require.Error(err, "cancelled slow query must return an error")
	assert.Less(elapsed, 30*time.Second,
		"cancelled query should abort promptly, not run to completion")
	assert.Error(ctx.Err(), "context should be cancelled")
}

// TestDuckDBQueryConcurrencyCap verifies the engine admits at most
// duckDBQueryConcurrency heavy queries at once and that a waiter respects its
// context: the third acquirer blocks while both slots are held and returns a
// context error, then succeeds once a slot is released.
func TestDuckDBQueryConcurrencyCap(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	require.Equal(2, duckDBQueryConcurrency, "test assumes a cap of 2")

	engine, err := NewDuckDBEngine("", "", nil)
	require.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })

	release1, err := engine.acquireQuerySlot(context.Background())
	require.NoError(err)
	release2, err := engine.acquireQuerySlot(context.Background())
	require.NoError(err)

	// Both slots held: a third acquirer must wait, and its context deadline
	// must free it rather than block forever.
	waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = engine.acquireQuerySlot(waitCtx)
	elapsed := time.Since(start)
	require.ErrorIs(err, context.DeadlineExceeded,
		"third acquire must fail with the waiter's context deadline")
	assert.Less(elapsed, 2*time.Second, "waiter should return near its deadline")

	// Freeing a slot lets a new acquirer through.
	release1()
	release3, err := engine.acquireQuerySlot(context.Background())
	require.NoError(err, "acquire must succeed after a slot is released")
	release3()
	release2()
}
