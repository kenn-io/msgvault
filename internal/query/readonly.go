package query

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrQueryNotReadOnly is returned by EnsureReadOnly when a raw SQL query is not
// a single read-only statement. The raw /api/v1/query endpoint is a
// trusted-user analytics interface, but it must not mutate the archive or the
// DuckDB session, so writes and multi-statement input are rejected before
// execution.
var ErrQueryNotReadOnly = errors.New("query not read-only")

// readOnlyCommands are the statement-leading keywords that only read data.
// DuckDB's `TABLE x` and `FROM x` shorthands and the describe/show/summarize
// family are all read-only. EXPLAIN and WITH are handled specially because the
// statement they wrap determines whether the whole statement mutates.
var readOnlyCommands = map[string]bool{
	"SELECT":    true,
	"WITH":      true,
	"FROM":      true,
	"VALUES":    true,
	"TABLE":     true,
	"DESCRIBE":  true,
	"DESC":      true,
	"SHOW":      true,
	"SUMMARIZE": true,
	"PIVOT":     true,
	"UNPIVOT":   true,
	"EXPLAIN":   true,
}

// commandKeywords is the set of statement-leading keywords we recognize while
// classifying a statement. It must include every mutating keyword so that, for
// example, DELETE in "DELETE FROM t" is matched before the FROM that follows
// it; otherwise a write could be misread as a read-only FROM query. Unknown
// leading keywords cause EnsureReadOnly to reject (fail closed).
var commandKeywords = mergeKeywordSets(readOnlyCommands, map[string]bool{
	"INSERT":     true,
	"UPDATE":     true,
	"DELETE":     true,
	"MERGE":      true,
	"UPSERT":     true,
	"CREATE":     true,
	"DROP":       true,
	"ALTER":      true,
	"TRUNCATE":   true,
	"ATTACH":     true,
	"DETACH":     true,
	"COPY":       true,
	"INSTALL":    true,
	"LOAD":       true,
	"FORCE":      true,
	"SET":        true,
	"RESET":      true,
	"PRAGMA":     true,
	"CALL":       true,
	"VACUUM":     true,
	"ANALYZE":    true,
	"CHECKPOINT": true,
	"EXPORT":     true,
	"IMPORT":     true,
	"USE":        true,
	"BEGIN":      true,
	"START":      true,
	"COMMIT":     true,
	"ROLLBACK":   true,
	"ABORT":      true,
	"PREPARE":    true,
	"EXECUTE":    true,
	"DEALLOCATE": true,
	"COMMENT":    true,
})

func mergeKeywordSets(sets ...map[string]bool) map[string]bool {
	merged := map[string]bool{}
	for _, set := range sets {
		for k := range set {
			merged[k] = true
		}
	}
	return merged
}

// EnsureReadOnly validates that sql contains exactly one statement and that the
// statement is read-only. It is comment- and string-literal-aware: comments are
// stripped, semicolons and keywords inside string literals or quoted
// identifiers are ignored, and the effective command of a WITH/EXPLAIN wrapper
// is classified by the statement it actually runs (so "WITH t AS (...) DELETE"
// is rejected). It fails closed: anything it cannot confidently classify as
// read-only is rejected.
func EnsureReadOnly(sql string) error {
	statements := splitStatements(sql)
	switch len(statements) {
	case 0:
		return fmt.Errorf("%w: no executable SQL statement found", ErrQueryNotReadOnly)
	case 1:
		// Single statement — the only accepted shape.
	default:
		return fmt.Errorf("%w: only a single statement is allowed, got %d", ErrQueryNotReadOnly, len(statements))
	}

	command, ok := effectiveCommand(statements[0])
	if !ok {
		return fmt.Errorf("%w: unable to classify statement; only read-only queries are allowed", ErrQueryNotReadOnly)
	}
	if !readOnlyCommands[command] {
		return fmt.Errorf(
			"%w: %s statements are not allowed; use SELECT, WITH, FROM, VALUES, TABLE, DESCRIBE, SHOW, SUMMARIZE, PIVOT, or EXPLAIN",
			ErrQueryNotReadOnly, command)
	}
	return nil
}

// splitStatements returns the non-empty, trimmed statements in sql, splitting on
// top-level semicolons. Comments are replaced with a space; string literals,
// quoted identifiers, and dollar-quoted strings are preserved so that
// semicolons inside them do not split a statement.
func splitStatements(sql string) []string {
	var (
		statements []string
		cur        strings.Builder
	)
	runes := []rune(sql)
	n := len(runes)
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			statements = append(statements, s)
		}
		cur.Reset()
	}
	for i := 0; i < n; {
		c := runes[i]
		switch {
		case c == '-' && i+1 < n && runes[i+1] == '-':
			i += 2
			for i < n && runes[i] != '\n' {
				i++
			}
			cur.WriteRune(' ')
		case c == '/' && i+1 < n && runes[i+1] == '*':
			i += 2
			for i+1 < n && (runes[i] != '*' || runes[i+1] != '/') {
				i++
			}
			i += 2
			cur.WriteRune(' ')
		case c == '\'' || c == '"':
			i = copyQuoted(&cur, runes, i, c)
		case c == '$':
			if end, ok := copyDollarQuoted(&cur, runes, i); ok {
				i = end
			} else {
				cur.WriteRune(c)
				i++
			}
		case c == ';':
			flush()
			i++
		default:
			cur.WriteRune(c)
			i++
		}
	}
	flush()
	return statements
}

// copyQuoted copies a quoted region (delimited by quote, doubled to escape)
// starting at the opening quote index i and returns the index just past the
// closing quote (or end of input for an unterminated literal).
func copyQuoted(dst *strings.Builder, runes []rune, i int, quote rune) int {
	n := len(runes)
	dst.WriteRune(runes[i])
	i++
	for i < n {
		if runes[i] == quote {
			if i+1 < n && runes[i+1] == quote {
				dst.WriteRune(quote)
				dst.WriteRune(quote)
				i += 2
				continue
			}
			dst.WriteRune(quote)
			return i + 1
		}
		dst.WriteRune(runes[i])
		i++
	}
	return i
}

// copyDollarQuoted handles PostgreSQL/DuckDB dollar-quoted strings ($tag$...$tag$
// or $$...$$). It returns the index past the closing tag and true when a valid
// opening tag begins at i, otherwise false (the caller treats $ literally).
func copyDollarQuoted(dst *strings.Builder, runes []rune, i int) (int, bool) {
	tag, tagLen, ok := dollarTag(runes, i)
	if !ok {
		return i, false
	}
	n := len(runes)
	dst.WriteString(tag)
	j := i + tagLen
	for j < n {
		if runes[j] == '$' {
			if candidate, candLen, valid := dollarTag(runes, j); valid && candidate == tag {
				dst.WriteString(candidate)
				return j + candLen, true
			}
		}
		dst.WriteRune(runes[j])
		j++
	}
	return n, true
}

// dollarTag reports whether runes[i:] begins with a dollar-quote tag ($ + optional
// identifier + $) and returns the tag text and its rune length.
func dollarTag(runes []rune, i int) (string, int, bool) {
	n := len(runes)
	if i >= n || runes[i] != '$' {
		return "", 0, false
	}
	j := i + 1
	for j < n && (runes[j] == '_' || unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j])) {
		j++
	}
	if j >= n || runes[j] != '$' {
		return "", 0, false
	}
	return string(runes[i : j+1]), (j + 1) - i, true
}

type wordToken struct {
	word  string
	depth int
}

// effectiveCommand returns the uppercased command keyword that determines
// whether a statement mutates state, unwrapping WITH (CTE) and EXPLAIN
// prefixes to the statement they run. ok is false when no command keyword can
// be identified.
func effectiveCommand(statement string) (string, bool) {
	tokens := tokenizeWords(statement)
	idx, command, ok := firstCommandKeyword(tokens, 0, false)
	if !ok {
		return "", false
	}
	// WITH and EXPLAIN wrap another statement. The real command is the next
	// command keyword at the top paren level (depth 0), so CTE bodies and
	// subqueries are skipped.
	for command == "WITH" || command == "EXPLAIN" {
		idx, command, ok = firstCommandKeyword(tokens, idx+1, true)
		if !ok {
			return "", false
		}
	}
	return command, true
}

// firstCommandKeyword returns the index, word, and ok for the first token at or
// after start whose word is a recognized command keyword. When requireTopLevel
// is true only tokens at paren depth 0 qualify.
func firstCommandKeyword(tokens []wordToken, start int, requireTopLevel bool) (int, string, bool) {
	for i := start; i < len(tokens); i++ {
		if requireTopLevel && tokens[i].depth != 0 {
			continue
		}
		if commandKeywords[tokens[i].word] {
			return i, tokens[i].word, true
		}
	}
	return 0, "", false
}

// tokenizeWords extracts uppercased identifier/keyword tokens with their paren
// depth, skipping the contents of string literals, quoted identifiers, and
// dollar-quoted strings so keywords inside them are never treated as commands.
func tokenizeWords(statement string) []wordToken {
	var tokens []wordToken
	runes := []rune(statement)
	n := len(runes)
	depth := 0
	for i := 0; i < n; {
		c := runes[i]
		switch {
		case c == '\'' || c == '"':
			i = skipQuoted(runes, i, c)
		case c == '$':
			if end, ok := skipDollarQuoted(runes, i); ok {
				i = end
			} else {
				i++
			}
		case c == '(':
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			i++
		case c == '_' || unicode.IsLetter(c):
			start := i
			for i < n && (runes[i] == '_' || unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i])) {
				i++
			}
			tokens = append(tokens, wordToken{
				word:  strings.ToUpper(string(runes[start:i])),
				depth: depth,
			})
		default:
			i++
		}
	}
	return tokens
}

func skipQuoted(runes []rune, i int, quote rune) int {
	n := len(runes)
	i++
	for i < n {
		if runes[i] == quote {
			if i+1 < n && runes[i+1] == quote {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

func skipDollarQuoted(runes []rune, i int) (int, bool) {
	tag, tagLen, ok := dollarTag(runes, i)
	if !ok {
		return i, false
	}
	n := len(runes)
	j := i + tagLen
	for j < n {
		if runes[j] == '$' {
			if candidate, candLen, valid := dollarTag(runes, j); valid && candidate == tag {
				return j + candLen, true
			}
		}
		j++
	}
	return n, true
}
