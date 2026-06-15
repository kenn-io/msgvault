package query

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
)

// TestPostgreSQLBuildFTSTerm asserts the PostgreSQL dialect renders a
// dialect-neutral term slice into a to_tsquery argument with prefix
// lexemes AND-joined by " & ", and that "stopword-like" words (e.g.
// "or") are emitted as ordinary lexemes rather than reinterpreted as a
// boolean operator. This is the unit-level guard for the hybrid FTS
// parity fix: the BM25 leg must prefix-match the SAME term set on PG as
// SQLite, and to_tsquery must never see a bare boolean operator from a
// natural-language query.
func TestPostgreSQLBuildFTSTerm(t *testing.T) {
	d := PostgreSQLQueryDialect{}

	cases := []struct {
		name    string
		terms   []string
		wantArg string
	}{
		{
			name:    "prefix lexemes AND-joined",
			terms:   []string{"security", "alert", "account"},
			wantArg: "security:* & alert:* & account:*",
		},
		{
			name:    "or is a lexeme not an operator",
			terms:   []string{"monthly", "bill", "or", "invoice"},
			wantArg: "monthly:* & bill:* & or:* & invoice:*",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assertpkg.New(t)
			expr, arg := d.BuildFTSTerm(tc.terms)
			assert.Equal("m.search_fts @@ to_tsquery('simple', ?)", expr)
			assert.Equal(tc.wantArg, arg)
		})
	}

	// "or" must appear as a prefix lexeme, proving it is NOT treated as a
	// tsquery boolean operator (which would corrupt the parity term set).
	_, arg := d.BuildFTSTerm([]string{"monthly", "bill", "or", "invoice"})
	assertpkg.Contains(t, arg, "or:*")
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
			assert := assertpkg.New(t)
			expr, arg := d.BuildFTSTerm(tc.terms)
			assert.Equal("messages_fts MATCH ?", expr)
			assert.Equal(tc.wantArg, arg)
		})
	}
}
