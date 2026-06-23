//go:build sqlite_vec && pgvector

package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// TestRunEmbed_PG_OpenAndZeroPending exercises the command-level PG wiring
// in the runEmbed path: opening the pgvector backend, creating a
// generation, and confirming that coverage reports 0 missing on an empty
// messages table (clean exit path). Skips when MSGVAULT_TEST_DB is unset
// or not a postgres DSN.
func TestRunEmbed_PG_OpenAndZeroPending(t *testing.T) {
	_, dsn := openServePGSchema(t)
	ctx := context.Background()

	// Open the store through the same helper the real code uses.
	st, err := store.Open(dsn)
	require.NoError(t, err, "store.Open")
	t.Cleanup(func() { _ = st.Close() })
	require.True(t, st.IsPostgreSQL(), "expected PG-backed store")

	// Open the pgvector backend — this runs the schema migration so that
	// index_generations and the embedding tables exist.
	pgb, err := pgvector.Open(ctx, pgvector.Options{
		DB:        st.DB(),
		Dimension: 4,
	})
	require.NoError(t, err, "pgvector.Open must succeed and migrate the schema")
	t.Cleanup(func() { _ = pgb.Close() })

	// A fresh isolated schema has no messages table yet. Create a minimal
	// one (with embed_gen) so the coverage gate query succeeds. Empty, so
	// coverage reports 0 missing.
	_, err = st.DB().ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS messages (
			id BIGINT PRIMARY KEY,
			deleted_at TIMESTAMPTZ,
			deleted_from_source_at TIMESTAMPTZ,
			embed_gen BIGINT
		)`)
	require.NoError(t, err, "create messages scaffold")

	// Exercise pickEmbedGeneration via its PG backend — same code path
	// runEmbed takes. With no messages, full-rebuild seeds 0 rows.
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{}
	cfg.Vector.Enabled = true
	cfg.Vector.Embeddings.Model = "test-model"
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Data.DatabaseURL = dsn

	gen, rebuildInProgress, err := pickEmbedGeneration(ctx, pgb, embedGenerationOpts{
		FullRebuild: true,
		Model:       cfg.Vector.Embeddings.Model,
		Dimension:   cfg.Vector.Embeddings.Dimension,
		Fingerprint: cfg.Vector.GenerationFingerprint(),
		Confirm:     func() bool { return true }, // auto-confirm
		Stderr:      openStderrSink(t),
	})
	require.NoError(t, err, "pickEmbedGeneration (full-rebuild) must succeed on PG")
	assert.NotZero(t, gen, "generation ID must be non-zero")
	assert.True(t, rebuildInProgress, "full-rebuild path must report rebuildInProgress=true")

	// MissingCount is what runEmbed uses to decide whether to activate
	// the generation. With an empty messages table missing must be 0.
	missing, err := st.MissingCount(ctx, int64(gen))
	require.NoError(t, err, "MissingCount on PG must succeed")
	assert.Equal(t, int64(0), missing, "empty messages table → 0 missing")

	// Confirm the generation state: still building (no activation yet).
	building, err := pgb.BuildingGeneration(ctx)
	require.NoError(t, err, "BuildingGeneration")
	require.NotNil(t, building, "expected a building generation")
	assert.Equal(t, vector.GenerationBuilding, building.State)
	assert.Equal(t, gen, building.ID)
}
