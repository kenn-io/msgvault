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
	"unicode/utf8"
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

// PostgreSQLSearchArg builds the unordered candidate query for the index.
func PostgreSQLSearchArg(terms []string) string {
	return tsQuery(terms, "", " & ")
}

// CanonicalFTSText prepares text for collation-independent phrase matching.
func CanonicalFTSText(s string) string {
	return strings.ToLower(strings.Join(EscapeTSQueryTerm(s), " "))
}

// PostgreSQLSearchPredicate narrows unordered candidates to ordered phrases.
func PostgreSQLSearchPredicate(arg string, terms []string) string {
	clauses := []string{"m.search_fts @@ to_tsquery('simple', " + arg + ")"}
	for _, term := range terms {
		if len(EscapeTSQueryTerm(term)) < 2 {
			continue
		}
		var weighted []string
		for _, weight := range []string{"A", "B", "C", "D"} {
			weighted = append(weighted, TSQuery([]string{term}, weight))
		}
		canonical := CanonicalFTSText(term)
		clauses = append(clauses, "((m.search_fts_fields IS NULL AND m.search_fts @@ to_tsquery('simple', '"+strings.Join(weighted, " | ")+"')) OR (m.search_fts_fields IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(m.search_fts_fields) AS f(value) WHERE strpos(' ' || f.value, ' "+canonical+"') > 0)))")
	}
	return strings.Join(clauses, " AND ")
}

// PostgreSQLBodyPredicate confirms every term against the body field.
func PostgreSQLBodyPredicate(arg string, terms []string) string {
	checks := []string{}
	for _, term := range terms {
		canonical := CanonicalFTSText(term)
		if canonical != "" {
			checks = append(checks, "strpos(' ' || m.search_fts_fields[5], ' "+canonical+"') > 0")
		}
	}
	if len(checks) == 0 {
		return "FALSE"
	}
	return "(m.search_fts @@ to_tsquery('simple', " + arg + ") AND ((m.search_fts_fields IS NULL AND m.search_fts @@ to_tsquery('simple', '" + TSQuery(terms, "D") + "')) OR (m.search_fts_fields IS NOT NULL AND " + strings.Join(checks, " AND ") + ")))"
}

func ftsTokenRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

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
		if ftsTokenRune(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return lexemes
}

// FoldFTSText maps each output byte boundary back to the original text.
func FoldFTSText(s string) (string, []int) {
	var b strings.Builder
	b.Grow(len(s))
	offsets := make([]int, 1, len(s)+1)
	for i, r := range s {
		end := i + utf8.RuneLen(r)
		length := end - i
		if ftsTokenRune(r) {
			length, _ = b.WriteRune(unicode.ToLower(r))
		} else {
			for range length {
				b.WriteByte(' ')
			}
		}
		for j := 1; j <= length; j++ {
			if j == length {
				offsets = append(offsets, end)
			} else {
				offsets = append(offsets, i)
			}
		}
	}
	return b.String(), offsets
}

// TSQuery builds ordered phrases, with a prefix on each final token.
func TSQuery(terms []string, weight string) string {
	return tsQuery(terms, weight, " <-> ")
}

func tsQuery(terms []string, weight, separator string) string {
	var groups []string
	for _, term := range terms {
		parts := EscapeTSQueryTerm(strings.ToLower(term))
		for i := range parts {
			if i == len(parts)-1 {
				parts[i] += ":*" + weight
			} else if weight != "" {
				parts[i] += ":" + weight
			}
		}
		if len(parts) == 1 {
			groups = append(groups, parts[0])
		} else if len(parts) > 1 {
			group := strings.Join(parts, separator)
			if separator != " & " {
				group = "(" + group + ")"
			}
			groups = append(groups, group)
		}
	}
	return strings.Join(groups, " & ")
}
