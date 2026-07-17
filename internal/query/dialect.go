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
	"time"

	"go.kenn.io/msgvault/internal/sqldialect"
	"go.kenn.io/msgvault/internal/sqliteutil"
	"go.kenn.io/msgvault/internal/store"
)

type messageBodyContextBackend uint8

const queryTimestampLayout = "2006-01-02 15:04:05.999999999"

func queryTimeUTC(value time.Time) time.Time {
	return value.UTC()
}

const (
	messageBodyContextSQLite messageBodyContextBackend = iota
	messageBodyContextPostgreSQL
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

	// FTSLivenessSQL returns a runtime liveness probe to run AFTER
	// HasFTSTableSQL confirms the FTS relation exists, or "" when the
	// existence probe is already authoritative.
	//
	// SQLite needs this: HasFTSTableSQL only checks sqlite_master for the
	// messages_fts virtual table, which does NOT prove the fts5 module is
	// loadable. A DB created by an fts5-enabled binary but opened by a
	// binary built without fts5 still has the row in sqlite_master, yet any
	// query against it fails with "no such module: fts5". The liveness probe
	// (`SELECT 1 FROM messages_fts LIMIT 1`) surfaces that so search falls
	// back to LIKE instead of erroring. This mirrors the store dialect's
	// FTSAvailable contract (internal/store/dialect_sqlite.go).
	//
	// PostgreSQL returns "" — its HasFTSTableSQL information_schema column
	// probe is an authoritative metadata check (the tsvector column either
	// exists and is queryable or it does not), so no extra liveness query
	// is needed.
	FTSLivenessSQL() string

	// FTSJoin returns a JOIN clause that must be added to the FROM clause
	// when using FTSSearchExpression. Empty string if no join is needed
	// (PostgreSQL has the tsvector column on messages directly).
	FTSJoin() string

	// BuildFTSTerm converts a slice of user-supplied search terms into a SQL
	// expression and a single argument string. Both SQLite FTS5 and PostgreSQL
	// tsquery support prefix matching via dialect-appropriate syntax.
	BuildFTSTerm(terms []string) (expr string, arg string)

	// BuildFTSBodyTerm is the exact-body counterpart to BuildFTSTerm.
	// SQLite scopes the FTS5 query to its body column; PostgreSQL restricts
	// tsquery lexemes to weight D in the versioned search_fts layout.
	BuildFTSBodyTerm(terms []string) (expr string, arg string)

	// FTSBodySearchReadinessSQL returns a query that yields true when the
	// backend's body-search layout is ready, or "" when no version probe is
	// needed. PostgreSQL uses messages.indexing_version; SQLite's FTS5 table
	// has a durable body column and needs no separate version watermark.
	FTSBodySearchReadinessSQL() string

	// SanitizeFTSQuery converts a raw user search string to a form safe to
	// pass to FTSSearchExpression. Returns "" if the result is empty after
	// sanitization (caller should treat as no-match).
	SanitizeFTSQuery(query string) string

	// BoolTrueExpr returns a SQL boolean expression that is true when col
	// holds a "true" value. SQLite stores booleans as 0/1 INTEGER so we
	// must emit "col = 1"; PostgreSQL has a real BOOLEAN type and rejects
	// integer comparisons, so the bare column name is the right form.
	BoolTrueExpr(col string) string

	// UnicodeLowerExpression returns a Unicode-aware lowercasing SQL expression.
	// SQLite delegates to a deterministic Go scalar registered on every
	// connection; PostgreSQL uses its collation-aware LOWER implementation.
	UnicodeLowerExpression(expr string) string

	// DateParam normalizes an instant for the backend timestamp representation.
	DateParam(value time.Time) any

	// DateComparison returns a backend-native comparison against one bound
	// placeholder. SQLite parses both operands as instants because archives can
	// contain mixed textual offsets; PostgreSQL compares typed timestamps.
	DateComparison(column, operator string) string

	// messageBodyContextBackend selects the backend-native highlighter used to
	// extract exact context for body-index hits.
	messageBodyContextBackend() messageBodyContextBackend
}

// SQLiteQueryDialect implements Dialect for SQLite.
type SQLiteQueryDialect struct{}

func (SQLiteQueryDialect) Rebind(query string) string { return query }

func (SQLiteQueryDialect) BoolTrueExpr(col string) string { return col + " = 1" }

func (SQLiteQueryDialect) UnicodeLowerExpression(expr string) string {
	return sqliteutil.UnicodeLowerFunction + "(" + expr + ")"
}

func (SQLiteQueryDialect) DateParam(value time.Time) any {
	return queryTimeUTC(value).Format(queryTimestampLayout)
}

func (SQLiteQueryDialect) DateComparison(column, operator string) string {
	return fmt.Sprintf("julianday(%s) %s julianday(?)", column, operator)
}

func (SQLiteQueryDialect) messageBodyContextBackend() messageBodyContextBackend {
	return messageBodyContextSQLite
}

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

// FTSLivenessSQL probes that the fts5 module is actually loadable, not just
// that the messages_fts row exists in sqlite_master. See the interface doc.
func (SQLiteQueryDialect) FTSLivenessSQL() string {
	return `SELECT 1 FROM messages_fts LIMIT 1`
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

func (d SQLiteQueryDialect) BuildFTSBodyTerm(terms []string) (expr string, arg string) {
	expr, arg = d.BuildFTSTerm(terms)
	if arg == "" {
		return expr, arg
	}
	return expr, "body : (" + arg + ")"
}

func (SQLiteQueryDialect) FTSBodySearchReadinessSQL() string { return "" }

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

func (PostgreSQLQueryDialect) UnicodeLowerExpression(expr string) string {
	return "LOWER(" + expr + ")"
}

func (PostgreSQLQueryDialect) DateParam(value time.Time) any {
	return queryTimeUTC(value)
}

func (PostgreSQLQueryDialect) DateComparison(column, operator string) string {
	return fmt.Sprintf("%s %s ?", column, operator)
}

func (PostgreSQLQueryDialect) messageBodyContextBackend() messageBodyContextBackend {
	return messageBodyContextPostgreSQL
}

func (PostgreSQLQueryDialect) TimeTruncExpression(column string, granularity string) string {
	switch granularity {
	case "year":
		return fmt.Sprintf("to_char(%s AT TIME ZONE 'UTC', 'YYYY')", column)
	case "month":
		return fmt.Sprintf("to_char(%s AT TIME ZONE 'UTC', 'YYYY-MM')", column)
	case "day":
		return fmt.Sprintf("to_char(%s AT TIME ZONE 'UTC', 'YYYY-MM-DD')", column)
	default:
		return fmt.Sprintf("to_char(%s AT TIME ZONE 'UTC', 'YYYY-MM')", column)
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
	// Scope to the connection's current schema (matching the store dialect's
	// postgresColumnExistsSQL). Without this, a schema-scoped connection would
	// falsely report FTS available because a sibling schema happens to have a
	// messages.search_fts column, then fail the actual search with
	// "column m.search_fts does not exist". [cr2-8]
	return `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'messages' AND column_name = 'search_fts'`
}

// FTSLivenessSQL is empty for PostgreSQL: the information_schema column
// probe in HasFTSTableSQL is already authoritative, and there is no
// messages_fts relation to probe (PG uses an inline search_fts tsvector
// column). Returning "" keeps the SQLite-only liveness query off the PG path.
func (PostgreSQLQueryDialect) FTSLivenessSQL() string { return "" }

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

func (PostgreSQLQueryDialect) BuildFTSBodyTerm(terms []string) (expr string, arg string) {
	termGroups := make([]string, 0, len(terms))
	for _, term := range terms {
		lexemes := sqldialect.EscapeTSQueryTerm(term)
		parts := make([]string, len(lexemes))
		for i, lexeme := range lexemes {
			suffix := ":D"
			if i == len(lexemes)-1 {
				suffix = ":*D"
			}
			parts[i] = lexeme + suffix
		}
		switch len(parts) {
		case 0:
			continue
		case 1:
			termGroups = append(termGroups, parts[0])
		default:
			termGroups = append(termGroups, "("+strings.Join(parts, " <-> ")+")")
		}
	}
	if len(termGroups) == 0 {
		return "FALSE", ""
	}
	return "m.search_fts @@ to_tsquery('simple', ?)", strings.Join(termGroups, " & ")
}

func (PostgreSQLQueryDialect) FTSBodySearchReadinessSQL() string {
	return fmt.Sprintf(
		"SELECT NOT EXISTS (SELECT 1 FROM messages WHERE search_fts IS NULL OR indexing_version IS DISTINCT FROM %d)",
		store.CurrentFTSIndexingVersion,
	)
}

// SanitizeFTSQuery builds a tsquery arg from a single user string using the
// allowlist tokenizer sqldialect.EscapeTSQueryTerm: the input is split on every
// rune that isn't a Unicode letter or digit, and each resulting lexeme is
// suffixed with ":*" for prefix matching and joined with " & ". This mirrors
// BuildFTSTerm exactly so both PG FTS paths emit the same lexeme set, and
// guarantees no tsquery metacharacter (`<`, `=`, `&`, etc.) ever reaches
// to_tsquery. Returns "" if the input collapses to nothing usable.
func (PostgreSQLQueryDialect) SanitizeFTSQuery(query string) string {
	var parts []string
	for _, lex := range sqldialect.EscapeTSQueryTerm(query) {
		parts = append(parts, lex+":*")
	}
	return strings.Join(parts, " & ")
}
