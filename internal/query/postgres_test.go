package query

import (
	"strings"
	"testing"
)

var _ Engine = (*SQLiteEngine)(nil)
var _ Engine = (*DuckDBEngine)(nil)

// TestPostgresEngineUsesDialect verifies that NewPostgreSQLEngine creates an engine
// with the PostgreSQL query dialect (Rebind converts ? to $N).
func TestPostgresEngineUsesDialect(t *testing.T) {
	e := NewPostgreSQLEngine(nil)
	pe, ok := e.(*pgEngine)
	if !ok {
		t.Fatalf("NewPostgreSQLEngine returned %T, want *pgEngine", e)
	}
	inner, ok := pe.Engine.(*SQLiteEngine)
	if !ok {
		t.Fatalf("pgEngine.Engine = %T, want *SQLiteEngine", pe.Engine)
	}
	if _, ok := inner.dialect.(PostgreSQLQueryDialect); !ok {
		t.Fatalf("inner dialect = %T, want PostgreSQLQueryDialect", inner.dialect)
	}
	reboundQuery := inner.dialect.Rebind("SELECT ? WHERE id = ?")
	if !strings.Contains(reboundQuery, "$1") || !strings.Contains(reboundQuery, "$2") {
		t.Fatalf("Rebind did not convert ? to $N: %q", reboundQuery)
	}
}

// TestPostgresEngineHidesTextEngine verifies that the PostgreSQL engine is
// NOT exposed as a TextEngine. The underlying *SQLiteEngine satisfies
// TextEngine, but the pgEngine wrapper deliberately hides those methods
// because they emit FTS5 MATCH and strftime() SQL that PostgreSQL rejects.
func TestPostgresEngineHidesTextEngine(t *testing.T) {
	e := NewPostgreSQLEngine(nil)
	if _, ok := e.(TextEngine); ok {
		t.Fatal("PostgreSQL engine must not satisfy TextEngine (SQLite-only FTS5/strftime SQL)")
	}
	// Sanity: the SQLite engine must still satisfy TextEngine.
	if _, ok := any(NewSQLiteEngine(nil)).(TextEngine); !ok {
		t.Fatal("SQLite engine should satisfy TextEngine")
	}
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
		if got != tc.want {
			t.Errorf("TimeTruncExpression(%q, %q) = %q, want %q", "col", tc.gran, got, tc.want)
		}
	}
}
