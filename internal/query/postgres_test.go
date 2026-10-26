package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ Engine = (*SQLiteEngine)(nil)
var _ Engine = (*DuckDBEngine)(nil)

// TestPostgresEngineHidesTextEngine verifies that the PostgreSQL engine is
// NOT exposed as a TextEngine. The underlying *SQLiteEngine satisfies
// TextEngine, but the pgEngine wrapper deliberately hides those methods
// because they emit FTS5 MATCH and strftime() SQL that PostgreSQL rejects.
func TestPostgresEngineHidesTextEngine(t *testing.T) {
	e := NewPostgreSQLEngine(nil)
	_, ok := e.(TextEngine)
	require.False(t, ok, "PostgreSQL engine must not satisfy TextEngine (SQLite-only FTS5/strftime SQL)")
	// Sanity: the SQLite engine must still satisfy TextEngine.
	_, ok = any(NewSQLiteEngine(nil)).(TextEngine)
	require.True(t, ok, "SQLite engine should satisfy TextEngine")
}

func TestPostgresEngineExposesAggregateDeletionResolver(t *testing.T) {
	requirements := require.New(t)
	engine := NewPostgreSQLEngine(nil)

	_, ok := engine.(DeletionTargetAggregateSearchResolver)
	requirements.True(ok, "PostgreSQL engine must forward aggregate deletion resolution")
}

// TestPostgresTimeTruncExpression verifies the PostgreSQL time truncation expressions.
func TestPostgresTimeTruncExpression(t *testing.T) {
	d := PostgreSQLQueryDialect{}
	for _, tc := range []struct {
		gran string
		want string
	}{
		{"year", "to_char(col AT TIME ZONE 'UTC', 'YYYY')"},
		{"month", "to_char(col AT TIME ZONE 'UTC', 'YYYY-MM')"},
		{"day", "to_char(col AT TIME ZONE 'UTC', 'YYYY-MM-DD')"},
	} {
		got := d.TimeTruncExpression("col", tc.gran)
		assert.Equal(t, tc.want, got, "TimeTruncExpression(%q, %q)", "col", tc.gran)
	}
}
