// Package sqldialect carries the small SQL primitives that both the
// store and query packages have to keep in lockstep so a query routed
// through one package matches a query routed through the other.
//
// Add things here only when divergence between the two packages would
// silently produce different results for the same input (e.g., a `?`
// rebind that doesn't respect quoted strings, or a tsquery escape that
// strips a different set of metacharacters). Dialect features that
// only one package needs (DDL, lifecycle, error classification) stay
// in that package's own Dialect interface.
package sqldialect

import (
	"fmt"
	"strings"
	"unicode"
)

// RebindPostgreSQL converts `?` placeholders in query to PostgreSQL
// `$1, $2, ...` numbered placeholders. `?` inside single-quoted string
// literals is left alone so prepared SQL fragments containing literal
// question marks survive the rewrite intact.
func RebindPostgreSQL(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 16)
	n := 1
	inQuote := false
	for i := range len(query) {
		ch := query[i]
		if ch == '\'' {
			inQuote = !inQuote
			b.WriteByte(ch)
		} else if ch == '?' && !inQuote {
			fmt.Fprintf(&b, "$%d", n)
			n++
		} else {
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// EscapeTSQueryTerm tokenizes a single user-supplied search term into
// PostgreSQL `to_tsquery`-safe lexemes. The input is split on every
// rune that isn't a Unicode letter or digit, so inputs that previously
// produced invalid tsquery fragments — `---` (parse error), `foo-bar`
// (the `-` is the NOT operator), `user@example.com`, `a.b.c` — now
// decompose into their component lexemes that to_tsquery accepts.
//
// Returns an empty slice when the input collapses to nothing usable
// (whitespace-only, all punctuation, or all metacharacters). Each
// returned lexeme is safe to suffix with `:*` for prefix matching and
// join with ` & `.
//
// This logic intentionally mirrors the broader punctuation-splitting
// behavior of query.PostgreSQLQueryDialect.SanitizeFTSQuery so the two
// PG FTS code paths produce the same lexeme set for the same input.
func EscapeTSQueryTerm(s string) []string {
	var lexemes []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			lexemes = append(lexemes, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return lexemes
}
