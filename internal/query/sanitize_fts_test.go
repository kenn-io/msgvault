package query

import (
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
	assert.Equal("3:*", got)
	assert.NotContains(got, "<")
	assert.NotContains(got, ">")

	// Inputs that are entirely metacharacters yield an empty arg.
	assert.Empty(d.SanitizeFTSQuery("<>=#$"))
	assert.Empty(d.SanitizeFTSQuery(""))
	assert.Empty(d.SanitizeFTSQuery("   "))
}
