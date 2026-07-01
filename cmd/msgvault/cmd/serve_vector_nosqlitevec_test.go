//go:build pgvector && !sqlite_vec

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

// TestPrecheckVectorFeatures_SQLiteWithoutSqliteVecTag verifies the cheap
// precheck fails fast when vector search is enabled against a SQLite
// mainPath but the binary was built with pgvector and WITHOUT the
// sqlite_vec tag. This mirrors the symmetric postgres-without-pgvector
// check: without it, a misconfigured SQLite + vector setup would only fail
// later in the background init goroutine (status=error) instead of at
// daemon startup. This test only compiles under pgvector && !sqlite_vec;
// under a sqlite_vec-tagged build, sqlitevec.Available() is true and this
// precheck must NOT fail, so the assertion would be wrong there.
func TestPrecheckVectorFeatures_SQLiteWithoutSqliteVecTag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	c.Vector.Embeddings.Endpoint = "http://localhost:11434/v1/embeddings"
	c.Vector.Embeddings.Model = "test-model"
	c.Vector.Embeddings.Dimension = 768
	withTestConfig(t, c)

	err := precheckVectorFeatures("msgvault.db")
	require.Error(err, "precheck must fail fast for sqlite mainPath without sqlite_vec tag")
	assert.Contains(err.Error(), "sqlite-vec",
		"error should point at the missing sqlite_vec build tag")
	assert.Contains(err.Error(), "enabled = false",
		"error should mention the config escape hatch")
}
