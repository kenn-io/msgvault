package query

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	// Inputs that are entirely metacharacters yield an empty arg.
	assert.Empty(d.SanitizeFTSQuery("<>=#$"))
	assert.Empty(d.SanitizeFTSQuery(""))
	assert.Empty(d.SanitizeFTSQuery("   "))
}

// TestPostgreSQLSanitizeFTSQuery_AllowlistOnly accepts only generated operators.
func TestPostgreSQLSanitizeFTSQuery_AllowlistOnly(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	d := PostgreSQLQueryDialect{}
	shape := regexp.MustCompile(`^(?:[\pL\pN]+:\*|\((?:[\pL\pN]+ <-> )+[\pL\pN]+:\*\))?$`)
	candidateShape := regexp.MustCompile(`^[\pL\pN]+(?::\*)?(?: & [\pL\pN]+(?::\*)?)*$`)
	patternShape := regexp.MustCompile(`^\(\^\| \)[\pL\pN]+(?: \+[\pL\pN]+)*$`)
	for _, in := range []string{"foo<3bar", "a=b", "c#d", "x$y", "user@example.com", "---", "a.b.c"} {
		arg := d.SanitizeFTSQuery(in)
		if arg == "" {
			continue
		}
		var payload struct {
			Phrase    string   `json:"phrase"`
			Candidate string   `json:"candidate"`
			Patterns  []string `json:"patterns"`
		}
		require.NoError(json.Unmarshal([]byte(arg), &payload))
		assert.Regexp(shape, payload.Phrase, "input %q", in)
		assert.Regexp(candidateShape, payload.Candidate, "input %q", in)
		require.NotEmpty(payload.Patterns)
		for _, pattern := range payload.Patterns {
			assert.Regexp(patternShape, pattern, "input %q", in)
		}
	}
}
