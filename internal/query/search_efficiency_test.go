//go:build cgo

package query

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
	"go.kenn.io/msgvault/internal/testutil/sqlobserve"
)

func observedSearchEngine(t *testing.T) (*SQLiteEngine, *sqlobserve.Observer) {
	t.Helper()
	db, observer := sqlobserve.Open(t, nil)
	schema, err := os.ReadFile("../store/schema.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(schema))
	require.NoError(t, err)
	fixture := &dbtest.TestDB{DB: db, T: t}
	fixture.SeedStandardDataSet()
	observer.Reset()
	return NewSQLiteEngine(db), observer
}

func TestMetadataSearchLegacyStatementBaseline(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		statements  int
	}{
		{"nonempty", "after:2024-01-01", 12},
		{"empty", "after:2099-01-01", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, observer := observedSearchEngine(t)
			q := search.Parse(tc.query)
			// Reconstruct the old daemon's effective limit=0 behavior (100 rows),
			// so this baseline remains accurate after count-only semantics are fixed.
			for _, limit := range []int{2, 100} {
				_, err := engine.SearchFastWithStats(t.Context(), q, tc.query, MessageFilter{}, ViewSenders, limit, 0)
				require.NoError(t, err)
			}
			assert.Len(t, observer.Statements(), tc.statements)
		})
	}
}

func TestMetadataSearchSQLiteOptionalStats(t *testing.T) {
	for _, tc := range []struct {
		name, query                     string
		limit, offset, rows, statements int
		total                           int64
	}{
		{"page", "after:2024-01-01", 2, 0, 2, 3, 5},
		{"end", "after:2024-01-01", 2, 9, 0, 2, 5},
		{"empty", "after:2099-01-01", 2, 0, 0, 2, 0},
		{"count only", "after:2024-01-01", 0, 0, 0, 1, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			engine, observer := observedSearchEngine(t)
			result, err := engine.SearchFastWithStats(t.Context(), search.Parse(tc.query), tc.query, MessageFilter{}, ViewNoStats, tc.limit, tc.offset)
			require.NoError(err)
			assert.Equal(tc.total, result.TotalCount)
			assert.Len(result.Messages, tc.rows)
			assert.Nil(result.Stats)
			assert.Len(observer.Statements(), tc.statements)
			for _, statement := range observer.Statements() {
				assert.NotContains(statement, "message_bodies")
			}
		})
	}
}

func TestMetadataSearchSQLiteCountCancellationStopsStats(t *testing.T) {
	engine, observer := observedSearchEngine(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observer.BeforeQuery = func(_ context.Context, sql string) {
		if strings.Contains(sql, "COUNT(DISTINCT m.id)") {
			cancel()
		}
	}
	_, err := engine.SearchFastWithStats(ctx, search.Parse("after:2024-01-01"), "after:2024-01-01", MessageFilter{}, ViewSenders, 2, 0)
	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, observer.Statements(), 3, "page, labels and canceled count only")
}
