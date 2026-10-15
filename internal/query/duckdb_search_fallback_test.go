package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func TestDuckDBSearchFallbackPipeOR(t *testing.T) {
	engine := NewTestDataBuilder(t).BuildEngine()
	require.Nil(t, engine.sqliteEngine)
	_, err := engine.Search(t.Context(), search.ParseOR("alpha | gamma"), 10, 0)
	assert.ErrorContains(t, err, "pipe OR requires a direct SQLite engine")
}
