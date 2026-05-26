package query

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

var _ Engine = (*SQLiteEngine)(nil)
var _ Engine = (*DuckDBEngine)(nil)

// TestPostgresEngineUsesDialect verifies that NewPostgreSQLEngine creates an engine
// with the PostgreSQL query dialect (Rebind converts ? to $N).
func TestPostgresEngineUsesDialect(t *testing.T) {
	require := requirepkg.New(t)
	e := NewPostgreSQLEngine(nil)
	pe, ok := e.(*pgEngine)
	require.True(ok, "NewPostgreSQLEngine returned %T, want *pgEngine", e)
	inner, ok := pe.Engine.(*SQLiteEngine)
	require.True(ok, "pgEngine.Engine = %T, want *SQLiteEngine", pe.Engine)
	_, ok = inner.dialect.(PostgreSQLQueryDialect)
	require.True(ok, "inner dialect = %T, want PostgreSQLQueryDialect", inner.dialect)
	reboundQuery := inner.dialect.Rebind("SELECT ? WHERE id = ?")
	require.Contains(reboundQuery, "$1", "Rebind did not convert ? to $N")
	require.Contains(reboundQuery, "$2", "Rebind did not convert ? to $N")
}

// TestPostgresEngineHidesTextEngine verifies that the PostgreSQL engine is
// NOT exposed as a TextEngine. The underlying *SQLiteEngine satisfies
// TextEngine, but the pgEngine wrapper deliberately hides those methods
// because they emit FTS5 MATCH and strftime() SQL that PostgreSQL rejects.
func TestPostgresEngineHidesTextEngine(t *testing.T) {
	e := NewPostgreSQLEngine(nil)
	_, ok := e.(TextEngine)
	requirepkg.False(t, ok, "PostgreSQL engine must not satisfy TextEngine (SQLite-only FTS5/strftime SQL)")
	// Sanity: the SQLite engine must still satisfy TextEngine.
	_, ok = any(NewSQLiteEngine(nil)).(TextEngine)
	requirepkg.True(t, ok, "SQLite engine should satisfy TextEngine")
}

// TestPostgresTimeTruncExpression verifies the PostgreSQL time truncation expressions.
func TestPostgresTimeTruncExpression(t *testing.T) {
	d := PostgreSQLQueryDialect{}
	for _, tc := range []struct {
		gran string
		want string
	}{
		{"year", "to_char(col, 'YYYY')"},
		{"month", "to_char(col, 'YYYY-MM')"},
		{"day", "to_char(col, 'YYYY-MM-DD')"},
	} {
		got := d.TimeTruncExpression("col", tc.gran)
		assertpkg.Equal(t, tc.want, got, "TimeTruncExpression(%q, %q)", "col", tc.gran)
	}
}
