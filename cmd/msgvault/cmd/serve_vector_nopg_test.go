//go:build sqlite_vec && !pgvector

package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// TestSetupVectorFeatures_PostgresWithoutPgvectorTag verifies that when
// vector features are built with sqlite_vec but WITHOUT the pgvector tag,
// invoking setupVectorFeatures against a postgres:// DSN fails from the
// pgvector stub (pgvector.Open → ErrNotBuilt), not from a hard-coded
// up-front refusal. The old "SQLite-only" refusal was removed when serve
// gained real PG vector support; this pins that no remaining code path
// emits it under this tag combo.
func TestSetupVectorFeatures_PostgresWithoutPgvectorTag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	cfg = &config.Config{}
	cfg.Vector.Enabled = true
	cfg.Vector.Backend = "sqlite-vec"
	cfg.Vector.Embeddings.Endpoint = "http://localhost:11434/v1/embeddings"
	cfg.Vector.Embeddings.Model = "test-model"
	cfg.Vector.Embeddings.Dimension = 768
	cfg.Vector.Embeddings.BatchSize = 32

	// setupVectorFeatures needs a non-nil *store.Store (it reads
	// store.DB()); an in-memory SQLite store suffices — the PG branch is
	// selected from the DSN and fails at the pgvector stub.
	st, err := store.Open(":memory:")
	require.NoError(
		err, "store.Open")

	t.Cleanup(func() { _ = st.Close() })

	_, err = setupVectorFeatures(context.Background(), st, "postgres://user@host/db", false)
	require.Error(err, "setupVectorFeatures with postgres DSN and no pgvector tag")
	assert. // Must come from the stub, not the removed up-front refusal.
		Contains(err.Error(), "pgvector support not compiled in",
			"error should be the pgvector stub's not-built message")
	assert.NotContains(err.Error(), "SQLite-only",
		"the old up-front SQLite-only refusal must be gone")
}
