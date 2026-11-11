package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPostgreSQLSanitizeFTSQuery_NoMetacharacters checks the prefix suffix
// after punctuation is removed from a search term.
func TestPostgreSQLSanitizeFTSQuery_NoMetacharacters(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	d := PostgreSQLQueryDialect{}

	// "<3" must collapse to the single lexeme "3:*" with no angle brackets.
	got := d.SanitizeFTSQuery("<3")
	assert.Equal("3:*", got)
}
