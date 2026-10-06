//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// TestRunEmbeddingsRetire_ForceActive drives the CLI retire path through the
// real sqlitevec backend (cf-2: the state transition routes through
// backend.RetireGeneration). It requires the sqlite_vec build tag because
// runEmbeddingsRetire opens a sqlitevec backend, whose RegisterExtension
// returns ErrNotBuilt under a no-sqlite_vec build. The untagged pre-check
// refusal lives in TestRetireEmbeddingGenerationRefusesActiveWithoutForce_PreCheck.
func TestRunEmbeddingsRetire_ForceActive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := newEmbeddingMetadataTestDBFile(t)
	testCtx := withEmbeddingCommandConfig(t, dbPath)

	oldYes := embeddingsRetireYes
	oldForce := embeddingsRetireForceActive
	embeddingsRetireYes = true
	embeddingsRetireForceActive = true
	t.Cleanup(func() {
		embeddingsRetireYes = oldYes
		embeddingsRetireForceActive = oldForce
	})

	cmd := embeddingsRetireCmd
	oldCtx := cmd.Context()
	cmd.SetContext(testCtx)
	t.Cleanup(func() { cmd.SetContext(oldCtx) })

	require.NoError(runEmbeddingsRetire(cmd, []string{"1"}),
		"retire active generation with --force-active")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })
	row := mustGetEmbeddingGeneration(t.Context(), t, db, 1)
	assert.Equal(vector.GenerationRetired, row.State)
}

func TestFillFullCoverageUsesEmbeddingScopeForEmbeddedCount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	dbPath := newEmbeddingMetadataTestDBFileAt(t, filepath.Join(dataDir, "vectors.db"))
	seedMainDBWithScopedFullCoverageMessages(t, dataDir)
	testCtx, cfg := withEmbeddingCommandConfigDataDir(t, dbPath, dataDir)
	cfg.Vector.Embed.Scope.MessageTypes = []string{"sms"}

	ctx := testCtx
	backend, closeBackend, err := openEmbeddingsBackend(ctx)
	require.NoError(err, "open embeddings backend")
	t.Cleanup(closeBackend)
	require.NoError(backend.Upsert(ctx, 2, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0, 0, 0}},
		{MessageID: 2, Vector: []float32{0, 1, 0, 0}},
	}), "upsert in-scope and out-of-scope vectors")

	row := embeddingGenerationRow{ID: 2}
	require.NoError(fillFullCoverage(ctx, backend, cfg.Vector.Embed.Scope.BuildScope(), &row))

	assert.Equal(int64(1), row.LiveCount, "only sms is in scope")
	assert.Equal(int64(1), row.EmbeddedCount, "out-of-scope email vector is excluded")
	assert.Equal(int64(0), row.BlankCount)
	assert.Equal(int64(0), row.MissingCount)
}

func TestFillFullCoverageKeepsSnapshotAcrossEmbeddingBatchCommit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	vecPath := newEmbeddingMetadataTestDBFileAt(t, filepath.Join(dataDir, "vectors.db"))
	main, err := store.Open(filepath.Join(dataDir, "msgvault.db"))
	require.NoError(err)
	require.NoError(main.InitSchema())
	_, err = main.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, embed_gen) VALUES
	(1, 1, 1, 'already-stamped', 'email', 2),
	(2, 1, 1, 'batch-pending', 'email', NULL);
`)
	require.NoError(err)
	require.NoError(main.Close())
	ctx, cfg := withEmbeddingCommandConfigDataDir(t, vecPath, dataDir)
	backend, closeBackend, err := openEmbeddingsBackend(ctx)
	require.NoError(err, "open embeddings backend")
	t.Cleanup(closeBackend)
	require.NoError(backend.Upsert(ctx, 2, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0, 0, 0}},
	}), "seed the already-stamped message vector")
	snapshot, ok := backend.(vector.CoverageSnapshotBackend)
	require.True(ok, "SQLite coverage backend supports a scoped message-ID intersection")

	interleaved := &interleavingCoverageBackend{
		Backend:  backend,
		snapshot: snapshot,
		commit: func(ctx context.Context) error {
			if err := backend.Upsert(ctx, 2, []vector.Chunk{
				{MessageID: 2, Vector: []float32{0, 1, 0, 0}},
			}); err != nil {
				return err
			}
			main, err := store.Open(cfg.DatabaseDSN())
			if err != nil {
				return err
			}
			defer func() { _ = main.Close() }()
			_, err = main.DB().ExecContext(ctx,
				`UPDATE messages SET embed_gen = ? WHERE id = ?`, 2, 2)
			return err
		},
	}

	row := embeddingGenerationRow{ID: 2}
	require.NoError(fillFullCoverage(ctx, interleaved, cfg.Vector.Embed.Scope.BuildScope(), &row))
	assert.Equal(int64(2), row.LiveCount)
	assert.Equal(int64(1), row.EmbeddedCount,
		"the message stamped after the main-DB snapshot is not part of its coverage count")
	assert.Equal(int64(0), row.BlankCount)
	assert.Equal(int64(1), row.MissingCount)
	assert.Equal(row.LiveCount, row.EmbeddedCount+row.BlankCount+row.MissingCount,
		"coverage remains a partition when an embedding batch commits between reads")
}

type interleavingCoverageBackend struct {
	vector.Backend

	snapshot vector.CoverageSnapshotBackend
	commit   func(context.Context) error
}

func (b *interleavingCoverageBackend) commitBeforeCount(ctx context.Context) error {
	if b.commit == nil {
		return nil
	}
	commit := b.commit
	b.commit = nil
	return commit(ctx)
}

func (b *interleavingCoverageBackend) EmbeddedMessageCountForSnapshot(
	ctx context.Context, gen vector.GenerationID, stampedMessageIDs []int64,
) (int64, error) {
	if err := b.commitBeforeCount(ctx); err != nil {
		return 0, err
	}
	return b.snapshot.EmbeddedMessageCountForSnapshot(ctx, gen, stampedMessageIDs)
}
