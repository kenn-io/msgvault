//go:build cgo

package query

import (
	"os"
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

func TestMetadataSearchSQLiteOptionalStats(t *testing.T) {
	t.Parallel()
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
