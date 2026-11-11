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
	"encoding/json/v2"
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

// PostgreSQLSearchArg carries both index queries and exact field patterns.
func PostgreSQLSearchArg(terms []string, weight string) string {
	phrase := TSQuery(terms, weight)
	if phrase == "" {
		return ""
	}
	var candidates, patterns []string
	for _, term := range terms {
		parts := EscapeTSQueryTerm(term)
		if len(parts) == 0 {
			continue
		}
		patterns = append(patterns, PostgreSQLPhrasePattern(term))
		parts[len(parts)-1] += ":*"
		candidates = append(candidates, strings.Join(parts, " & "))
	}
	data, _ := json.Marshal(struct {
		Phrase    string   `json:"phrase"`
		Candidate string   `json:"candidate"`
		Patterns  []string `json:"patterns"`
	}{phrase, strings.Join(candidates, " & "), patterns})
	return string(data)
}

// PostgreSQLPhrasePattern matches adjacent normalized pieces with a final prefix.
func PostgreSQLPhrasePattern(term string) string {
	parts := EscapeTSQueryTerm(term)
	if len(parts) == 0 {
		return ""
	}
	return "(^| )" + strings.Join(parts, " +")
}

// PostgreSQLSearchPredicate binds one search argument and checks each term in a field.
func PostgreSQLSearchPredicate(arg, fields string) string {
	fields = strings.ReplaceAll(fields, "m.", "indexed.")
	return `m.id IN (WITH fts AS MATERIALIZED (SELECT * FROM jsonb_to_record(` + arg + `::jsonb) AS payload(phrase text, candidate text, patterns text[]))
		SELECT indexed.id FROM fts CROSS JOIN messages indexed WHERE
		(indexed.search_fts_fields IS NULL AND indexed.search_fts @@ to_tsquery('simple', fts.phrase)) OR ` + compactFTSMatch(fields) + ` OR
		(cardinality(indexed.search_fts_fields) > 3 AND indexed.search_fts @@ to_tsquery('simple', fts.candidate) AND
		 NOT EXISTS (SELECT 1 FROM unnest(fts.patterns) AS pattern WHERE NOT EXISTS
		 (SELECT 1 FROM unnest(` + fields + `) AS field WHERE field ~* pattern))))`
}

// Three fields hold legacy text; primary fields still have complete positions.
// TSQuery joins one phrase per term with " & "; normalized terms cannot contain it.
func compactFTSMatch(fields string) string {
	return `(cardinality(indexed.search_fts_fields) = 3 AND indexed.search_fts @@ to_tsquery('simple', fts.candidate) AND
		NOT EXISTS (SELECT 1 FROM unnest(fts.patterns, string_to_array(fts.phrase, ' & ')) AS term(pattern, phrase)
		WHERE NOT (indexed.search_fts @@ to_tsquery('simple', term.phrase) OR EXISTS (SELECT 1 FROM unnest(` + fields + `) AS field WHERE field ~* term.pattern))))`
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

// TSQuery builds ordered phrases, with a prefix on each final token.
func TSQuery(terms []string, weight string) string {
	var groups []string
	for _, term := range terms {
		parts := EscapeTSQueryTerm(term)
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
