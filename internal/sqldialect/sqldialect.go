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
	var groups []string
	for _, term := range terms {
		parts := EscapeTSQueryTerm(strings.ToLower(term))
		if len(parts) > 0 {
			parts[len(parts)-1] += ":*"
		}
		groups = append(groups, parts...)
	}
	return strings.Join(groups, " & ")
}

// CanonicalFTSText prepares text for collation-independent phrase matching.
func CanonicalFTSText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(NormalizeFTSText(s))), " ")
}

// PostgreSQLSearchPredicate narrows unordered candidates to ordered phrases.
func PostgreSQLSearchPredicate(arg string, terms []string) string {
	clauses := []string{"m.search_fts @@ to_tsquery('simple', " + arg + ")"}
	for _, term := range terms {
		if len(EscapeTSQueryTerm(term)) < 2 {
			continue
		}
		// TSQuery contains only letters and digits, so it is safe to inline.
		phrase := TSQuery([]string{term}, "")
		clauses = append(clauses, "(m.search_fts @@ to_tsquery('simple', '"+phrase+"') OR m.search_fts @@ '''!!'''::tsquery)")
	}
	return strings.Join(clauses, " AND ")
}

// PostgreSQLBodyPredicate confirms every term against a saturated body copy.
func PostgreSQLBodyPredicate(arg string, terms []string) string {
	checks := []string{}
	for _, term := range terms {
		canonical := CanonicalFTSText(term)
		if canonical != "" {
			checks = append(checks, "strpos(' ' || m.search_fts_body, ' "+canonical+"') > 0")
		}
	}
	if len(checks) == 0 {
		return "FALSE"
	}
	return "(m.search_fts @@ to_tsquery('simple', " + arg + ") AND ((m.search_fts_body IS NULL AND m.search_fts @@ to_tsquery('simple', '" + TSQuery(terms, "D") + "')) OR (m.search_fts_body IS NOT NULL AND " + strings.Join(checks, " AND ") + ")))"
}

// tokenSpans returns byte offsets of Unicode letter and digit runs.
func tokenSpans(s string) [][2]int {
	var spans [][2]int
	start := -1
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			spans = append(spans, [2]int{start, i})
			start = -1
		}
	}
	if start >= 0 {
		spans = append(spans, [2]int{start, len(s)})
	}
	return spans
}

// EscapeTSQueryTerm splits a search term into Unicode letter and digit runs.
func EscapeTSQueryTerm(s string) []string {
	var lexemes []string
	for _, span := range tokenSpans(s) {
		lexemes = append(lexemes, s[span[0]:span[1]])
	}
	return lexemes
}

// NormalizeFTSText replaces separators with spaces without changing byte offsets.
func NormalizeFTSText(s string) string {
	b := []byte(s)
	for i := range b {
		b[i] = ' '
	}
	for _, span := range tokenSpans(s) {
		copy(b[span[0]:span[1]], s[span[0]:span[1]])
	}
	return string(b)
}

// FoldFTSText maps each output byte boundary back to the original text.
func FoldFTSText(s string) (string, []int) {
	var b strings.Builder
	b.Grow(len(s))
	offsets := make([]int, 1, len(s)+1)
	for i, r := range s {
		end := i + utf8.RuneLen(r)
		length := end - i
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
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
			groups = append(groups, "("+strings.Join(parts, " <-> ")+")")
		}
	}
	return strings.Join(groups, " & ")
}
