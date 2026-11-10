package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPostgreSQLBuildFTSTerm(t *testing.T) {
	t.Parallel()
	expr, arg := (PostgreSQLQueryDialect{}).BuildFTSTerm([]string{"monthly", "bill", "or", "invoice"})
	assert.Contains(t, expr, "indexed.search_fts_fields")
	assert.JSONEq(t, `{"phrase":"monthly:* & bill:* & or:* & invoice:*","candidate":"monthly:* & bill:* & or:* & invoice:*","patterns":["(^| )monthly","(^| )bill","(^| )or","(^| )invoice"]}`, arg)
}

// TestSQLiteBuildFTSTerm asserts the SQLite dialect renders a
// dialect-neutral term slice into an FTS5 MATCH argument: each term is
// double-quote-wrapped with a trailing "*" for prefix matching, embedded
// double-quotes are doubled (FTS5 escaping that neutralizes operator
// injection), and stray "*" inside a term is stripped. This is the
// injection-relevant counterpart to TestPostgreSQLBuildFTSTerm and keeps
// dialect.go's FTS5 escaping (quote-doubling, star-stripping) under
// direct test now that the hybrid path renders terms per-dialect.
func TestSQLiteBuildFTSTerm(t *testing.T) {
	t.Parallel()
	d := SQLiteQueryDialect{}

	cases := []struct {
		name    string
		terms   []string
		wantArg string
	}{
		{
			name:    "plain terms quote-wrapped and prefix-matched",
			terms:   []string{"security", "alert"},
			wantArg: `"security"* "alert"*`,
		},
		{
			name:    "embedded double-quote doubled",
			terms:   []string{`a"b`},
			wantArg: `"a""b"*`,
		},
		{
			name:    "stray star stripped",
			terms:   []string{"a*b"},
			wantArg: `"ab"*`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			expr, arg := d.BuildFTSTerm(tc.terms)
			assert.Equal("messages_fts MATCH ?", expr)
			assert.Equal(tc.wantArg, arg)
		})
	}
}

func TestBuildFTSBodyTermScopesExactBodyField(t *testing.T) {
	t.Parallel()
	t.Run("PostgreSQL weight D", func(t *testing.T) {
		expr, arg := (PostgreSQLQueryDialect{}).BuildFTSBodyTerm([]string{"foo-bar", "baz"})
		assert.Contains(t, expr, "indexed.search_fts_fields[5:5]")
		assert.JSONEq(t, `{"phrase":"(foo:D <-> bar:*D) & baz:*D","candidate":"foo & bar:* & baz:*","patterns":["(^| )foo +bar","(^| )baz"]}`, arg)
	})

	t.Run("SQLite body column", func(t *testing.T) {
		expr, arg := (SQLiteQueryDialect{}).BuildFTSBodyTerm([]string{"foo", "bar"})
		assert.Equal(t, "messages_fts MATCH ?", expr)
		assert.Equal(t, `body : ("foo"* "bar"*)`, arg)
	})
}
