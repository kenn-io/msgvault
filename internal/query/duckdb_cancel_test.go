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

	engine, err := NewDuckDBEngine("", "", nil)
	require.NoError(t, err)
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

	require.Error(t, err, "cancelled slow query must return an error")
	assert.Less(t, elapsed, 30*time.Second,
		"cancelled query should abort promptly, not run to completion")
	assert.Error(t, ctx.Err(), "context should be cancelled")
}
