// Package query - database dialect abstraction for query engine.
//
// The query engine uses a small dialect interface to handle SQLite vs.
// PostgreSQL differences that surface in aggregate/search SQL:
//   - ? vs $N placeholder syntax (Rebind)
//   - strftime vs to_char for time truncation
//   - messages_fts MATCH vs tsvector @@ for full-text search
//   - sqlite_master vs information_schema for existence probes
//
// The store package has a richer Dialect interface for its own needs;
// this package maintains a minimal parallel abstraction to avoid a
// cross-package dependency.

package query

import (
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/sqldialect"
)

// Dialect abstracts SQL generation differences for SQLite vs PostgreSQL.
type Dialect interface {
	// Rebind converts ? placeholders to the driver's native form.
	// No-op for SQLite; converts to $1, $2, ... for PostgreSQL.
	Rebind(query string) string

	// TimeTruncExpression returns SQL to truncate a timestamp column to a
	// given granularity ("year", "month", "day"). Used in GROUP BY for
	// the Time aggregate view.
	TimeTruncExpression(column string, granularity string) string

	// FTSSearchExpression returns the SQL boolean expression (with a ?
	// placeholder for the search term) to use in a WHERE clause for
	// full-text search. SQLite: messages_fts MATCH; PostgreSQL: tsvector @@.
	FTSSearchExpression() string

	// HasFTSTableSQL returns SQL to probe whether the FTS index exists.
	// Returns a single-row, single-column integer: 1 if present, 0 if absent.
	HasFTSTableSQL() string

	// FTSJoin returns a JOIN clause that must be added to the FROM clause
	// when using FTSSearchExpression. Empty string if no join is needed
	// (PostgreSQL has the tsvector column on messages directly).
	FTSJoin() string

	// BuildFTSTerm converts a slice of user-supplied search terms into a SQL
	// expression and a single argument string. Both SQLite FTS5 and PostgreSQL
	// tsquery support prefix matching via dialect-appropriate syntax.
	BuildFTSTerm(terms []string) (expr string, arg string)

	// SanitizeFTSQuery converts a raw user search string to a form safe to
	// pass to FTSSearchExpression. Returns "" if the result is empty after
	// sanitization (caller should treat as no-match).
	SanitizeFTSQuery(query string) string

	// BoolTrueExpr returns a SQL boolean expression that is true when col
	// holds a "true" value. SQLite stores booleans as 0/1 INTEGER so we
	// must emit "col = 1"; PostgreSQL has a real BOOLEAN type and rejects
	// integer comparisons, so the bare column name is the right form.
	BoolTrueExpr(col string) string
}

// SQLiteQueryDialect implements Dialect for SQLite.
type SQLiteQueryDialect struct{}

func (SQLiteQueryDialect) Rebind(query string) string { return query }

func (SQLiteQueryDialect) BoolTrueExpr(col string) string { return col + " = 1" }

func (SQLiteQueryDialect) TimeTruncExpression(column string, granularity string) string {
	switch granularity {
	case "year":
		return fmt.Sprintf("strftime('%%Y', %s)", column)
	case "month":
		return fmt.Sprintf("strftime('%%Y-%%m', %s)", column)
	case "day":
		return fmt.Sprintf("strftime('%%Y-%%m-%%d', %s)", column)
	default:
		return fmt.Sprintf("strftime('%%Y-%%m', %s)", column)
	}
}

func (SQLiteQueryDialect) FTSSearchExpression() string {
	return "messages_fts MATCH ?"
}

func (SQLiteQueryDialect) HasFTSTableSQL() string {
	return `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='messages_fts'`
}

func (SQLiteQueryDialect) FTSJoin() string {
	return "JOIN messages_fts fts ON fts.rowid = m.id"
}

// BuildFTSTerm for SQLite FTS5: quote each term and add "*" for prefix match,
// AND them together. Escaping double-quotes prevents injection of FTS5 operators.
func (SQLiteQueryDialect) BuildFTSTerm(terms []string) (expr string, arg string) {
	ftsTerms := make([]string, len(terms))
	for i, term := range terms {
		term = strings.ReplaceAll(term, "\"", "\"\"")
		term = strings.ReplaceAll(term, "*", "")
		ftsTerms[i] = fmt.Sprintf("\"%s\"*", term)
	}
	return "messages_fts MATCH ?", strings.Join(ftsTerms, " ")
}

// SanitizeFTSQuery strips FTS5 metacharacters from a single query string
// and wraps it in quotes for literal phrase interpretation with prefix match.
func (SQLiteQueryDialect) SanitizeFTSQuery(query string) string {
	var b strings.Builder
	for _, r := range query {
		switch r {
		case '"', '*', ':', '-', '(', ')', '.':
			continue
		default:
			b.WriteRune(r)
		}
	}
	clean := strings.TrimSpace(b.String())
	if clean == "" {
		return ""
	}
	return `"` + clean + `"*`
}

// PostgreSQLQueryDialect implements Dialect for PostgreSQL.
type PostgreSQLQueryDialect struct{}

// Rebind converts ? placeholders to $1, $2, ... for PostgreSQL.
// Delegates to sqldialect so store.PostgreSQLDialect.Rebind stays in
// lockstep — divergence here would route the same query to two
// different rebinds depending on which package owns the call site.
func (PostgreSQLQueryDialect) Rebind(query string) string {
	return sqldialect.RebindPostgreSQL(query)
}

func (PostgreSQLQueryDialect) BoolTrueExpr(col string) string { return col }

func (PostgreSQLQueryDialect) TimeTruncExpression(column string, granularity string) string {
	switch granularity {
	case "year":
		return fmt.Sprintf("to_char(%s, 'YYYY')", column)
	case "month":
		return fmt.Sprintf("to_char(%s, 'YYYY-MM')", column)
	case "day":
		return fmt.Sprintf("to_char(%s, 'YYYY-MM-DD')", column)
	default:
		return fmt.Sprintf("to_char(%s, 'YYYY-MM')", column)
	}
}

// FTSSearchExpression uses to_tsquery (not plainto_tsquery) so the
// bound argument can carry prefix-match operators ("invo:*" matches
// "invoice"); BuildFTSTerm and SanitizeFTSQuery both emit arguments in
// that shape, and the store dialect's FTSSearchClause does the same.
// Keeping all three aligned prevents the next caller from binding a
// :*-shaped argument into plainto_tsquery and silently getting a
// literal-phrase match.
func (PostgreSQLQueryDialect) FTSSearchExpression() string {
	return "m.search_fts @@ to_tsquery('simple', ?)"
}

func (PostgreSQLQueryDialect) HasFTSTableSQL() string {
	return `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'messages' AND column_name = 'search_fts'`
}

// FTSJoin: PostgreSQL's tsvector column lives on messages — no join needed.
func (PostgreSQLQueryDialect) FTSJoin() string { return "" }

// BuildFTSTerm for PostgreSQL to_tsquery: tokenize each user term into
// letter/digit-only lexemes via sqldialect.EscapeTSQueryTerm (shared
// with store.PostgreSQLDialect) so punctuation like `-`, `.`, `@`
// becomes a lexeme boundary rather than ending up in an invalid
// tsquery, append :* for prefix match, AND lexemes with " & ".
func (PostgreSQLQueryDialect) BuildFTSTerm(terms []string) (expr string, arg string) {
	tsTerms := make([]string, 0, len(terms))
	for _, term := range terms {
		for _, lex := range sqldialect.EscapeTSQueryTerm(term) {
			tsTerms = append(tsTerms, lex+":*")
		}
	}
	if len(tsTerms) == 0 {
		return "FALSE", ""
	}
	return "m.search_fts @@ to_tsquery('simple', ?)", strings.Join(tsTerms, " & ")
}

// SanitizeFTSQuery builds a tsquery arg from a single user string: splits on
// whitespace, strips tsquery metacharacters, and joins with " & " with ":*"
// prefix matching. Returns "" if empty.
func (PostgreSQLQueryDialect) SanitizeFTSQuery(query string) string {
	var b strings.Builder
	for _, r := range query {
		switch r {
		case '&', '|', '!', '(', ')', ':', '*', '\\', '\'':
			continue
		case '@', '.', '-', '/', ',', ';', '"':
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	tokens := strings.Fields(b.String())
	if len(tokens) == 0 {
		return ""
	}
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		parts = append(parts, t+":*")
	}
	return strings.Join(parts, " & ")
}
