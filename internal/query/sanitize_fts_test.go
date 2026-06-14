package query

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/sqldialect"
)

// TestPostgreSQLSanitizeFTSQuery_NoMetacharacters verifies that the PostgreSQL
// SanitizeFTSQuery tokenizer is allowlist-based: no tsquery metacharacter (such
// as `<`, `>`, `=`, `#`, `$`) ever survives into the to_tsquery argument. A
// stray `<` would open tsquery's phrase operator and raise SQLSTATE 42601.
func TestPostgreSQLSanitizeFTSQuery_NoMetacharacters(t *testing.T) {
	d := PostgreSQLQueryDialect{}

	// "<3" must collapse to the single lexeme "3:*" with no angle brackets.
	got := d.SanitizeFTSQuery("<3")
	assert.NotContains(t, got, "<")
	assert.NotContains(t, got, ">")
	assert.Equal(t, "3:*", got)

	// Inputs that are entirely metacharacters yield an empty arg.
	assert.Empty(t, d.SanitizeFTSQuery("<>=#$"))
	assert.Empty(t, d.SanitizeFTSQuery(""))
	assert.Empty(t, d.SanitizeFTSQuery("   "))
}

// TestPostgreSQLSanitizeFTSQuery_AllowlistOnly asserts the output never contains
// any character outside the allowed set: Unicode letters/digits plus the join
// punctuation ":*" and " & " that SanitizeFTSQuery itself adds.
func TestPostgreSQLSanitizeFTSQuery_AllowlistOnly(t *testing.T) {
	d := PostgreSQLQueryDialect{}
	for _, in := range []string{"foo<3bar", "a=b", "c#d", "x$y", "user@example.com", "---", "a.b.c"} {
		got := d.SanitizeFTSQuery(in)
		// Strip the join/suffix punctuation the sanitizer is allowed to emit,
		// then confirm everything left is a letter or digit.
		stripped := strings.NewReplacer(":", "", "*", "", "&", "", " ", "").Replace(got)
		for _, r := range stripped {
			assert.True(t, isLetterOrDigit(r),
				"input %q produced disallowed rune %q in output %q", in, string(r), got)
		}
	}
}

// TestPostgreSQLSanitizeFTSQuery_ParityWithEscapeTSQueryTerm asserts that
// SanitizeFTSQuery and the live BuildFTSTerm path (via EscapeTSQueryTerm)
// produce the same lexeme set for the same input, so the two PG FTS code
// paths cannot diverge.
func TestPostgreSQLSanitizeFTSQuery_ParityWithEscapeTSQueryTerm(t *testing.T) {
	d := PostgreSQLQueryDialect{}
	for _, in := range []string{"foo<3bar", "a=b", "c#d", "invoice 2024", "user@example.com"} {
		want := make([]string, 0)
		for _, lex := range sqldialect.EscapeTSQueryTerm(in) {
			want = append(want, lex+":*")
		}
		wantArg := strings.Join(want, " & ")
		assert.Equal(t, wantArg, d.SanitizeFTSQuery(in),
			"SanitizeFTSQuery(%q) must match the EscapeTSQueryTerm lexeme set", in)
	}
}

func isLetterOrDigit(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
