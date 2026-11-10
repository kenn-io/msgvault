package query

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPostgreSQLSanitizeFTSQuery_NoMetacharacters verifies that the PostgreSQL
// SanitizeFTSQuery tokenizer is allowlist-based: no tsquery metacharacter (such
// as `<`, `>`, `=`, `#`, `$`) ever survives into the to_tsquery argument. A
// stray `<` would open tsquery's phrase operator and raise SQLSTATE 42601.
func TestPostgreSQLSanitizeFTSQuery_NoMetacharacters(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	d := PostgreSQLQueryDialect{}

	// "<3" must collapse to the single lexeme "3:*" with no angle brackets.
	got := d.SanitizeFTSQuery("<3")
	assert.NotContains(got, "<")
	assert.NotContains(got, ">")
	assert.Equal("3:*", got)

	// Inputs that are entirely metacharacters yield an empty arg.
	assert.Empty(d.SanitizeFTSQuery("<>=#$"))
	assert.Empty(d.SanitizeFTSQuery(""))
	assert.Empty(d.SanitizeFTSQuery("   "))
}

// TestPostgreSQLSanitizeFTSQuery_AllowlistOnly accepts only generated operators.
func TestPostgreSQLSanitizeFTSQuery_AllowlistOnly(t *testing.T) {
	t.Parallel()
	d := PostgreSQLQueryDialect{}
	shape := regexp.MustCompile(`^(?:[\pL\pN]+:\*|\((?:[\pL\pN]+ <-> )+[\pL\pN]+:\*\))?$`)
	for _, in := range []string{"foo<3bar", "a=b", "c#d", "x$y", "user@example.com", "---", "a.b.c"} {
		assert.Regexp(t, shape, d.SanitizeFTSQuery(in), "input %q", in)
	}
}
