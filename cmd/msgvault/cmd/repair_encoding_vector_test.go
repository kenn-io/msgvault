//go:build sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// embedGenBackfillMigrationKey duplicates the (unexported) sqlitevec ledger key
// that guards the one-time upgrade backfill. Kept in sync deliberately: the
// test reconstructs the fresh-upgrade precondition by clearing this exact row.
const embedGenBackfillMigrationKey = "embed_gen_backfill_active_v1"

// readEmbedGen reports embed_gen for a message and whether it is NULL.
func readEmbedGen(t *testing.T, db *sql.DB, id int64) (val int64, isNull bool) {
	t.Helper()
	var v sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT embed_gen FROM messages WHERE id = ?`, id).Scan(&v))
	return v.Int64, !v.Valid
}

// TestRepairResetEmbeddings_OpensBackendBeforeResettingEmbedGen is the FIX A
// regression guard (review MEDIUM): repair-encoding must OPEN the vector backend
// (which runs the one-time upgrade backfill as a side effect) BEFORE it clears
// embed_gen on the repaired messages.
//
// Precondition reproduced: a freshly-upgraded archive from a pre-embed_gen
// build — an ACTIVE generation with existing active-gen embeddings, embed_gen
// NULL everywhere, and the backfill ledger UNMARKED. The first writable open of
// the vector backend runs BackfillEmbedGenForUpgrade, which stamps
// embed_gen=active on every already-embedded message.
//
// The bug: with the OLD ordering (ResetEmbedGen first, then open the backend to
// lower the watermark), the very first backend open during repair runs the
// backfill AFTER the reset, re-stamping the just-NULLed (previously-embedded)
// repaired message back to embed_gen=active — silently undoing the re-embed
// request. The fix opens the backend first so the backfill lands and marks its
// ledger BEFORE the reset, so the NULL sticks.
//
// Assert: the repaired message ends embed_gen IS NULL (it will be re-embedded).
// This FAILS with the old reset-before-open ordering (the message ends stamped
// =active) and PASSES with the open-before-reset fix.
func TestRepairResetEmbeddings_OpensBackendBeforeResettingEmbedGen(t *testing.T) {
	require.NoError(t, sqlitevec.RegisterExtension(), "RegisterExtension")
	ctx := context.Background()

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	vecPath := filepath.Join(dir, "vectors.db")

	// Real main DB with two live messages, both previously embedded.
	s, err := store.Open(mainPath)
	require.NoError(t, err, "store.Open")
	// Close the store LAST (registered first → LIFO) so the backend that
	// borrows s.DB() closes before it, and the open msgvault.db handle does not
	// block t.TempDir() cleanup on Windows.
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.InitSchema(), "InitSchema")
	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type)
VALUES (1, 1, 1, 'm1', 'email'), (2, 1, 1, 'm2', 'email');
`)
	require.NoError(t, err, "seed messages")

	// Create + activate a generation with an embedding for BOTH messages, so
	// they are genuinely "previously embedded" (the upgrade-backfill target).
	rw, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path: vecPath, MainPath: mainPath, Dimension: 4, MainDB: s.DB(),
	})
	require.NoError(t, err, "rw backend Open")
	gen, err := rw.CreateGeneration(ctx, "model", 4, "model:4")
	require.NoError(t, err, "CreateGeneration")
	require.NoError(t, rw.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: 1, ChunkIndex: 0, Vector: []float32{0, 0, 0, 1}},
		{MessageID: 2, ChunkIndex: 0, Vector: []float32{0, 0, 1, 0}},
	}), "Upsert")
	require.NoError(t, rw.ActivateGeneration(ctx, gen, true), "Activate")
	require.NoError(t, rw.Close(), "close rw backend")

	// Reconstruct the fresh-upgrade precondition: embed_gen NULL everywhere and
	// the backfill ledger UNMARKED, so the NEXT writable open runs the backfill.
	_, err = s.DB().Exec(`UPDATE messages SET embed_gen = NULL`)
	require.NoError(t, err, "reset embed_gen")
	_, err = s.DB().Exec(`DELETE FROM applied_migrations WHERE name = ?`, embedGenBackfillMigrationKey)
	require.NoError(t, err, "clear backfill ledger")

	// Wire cfg so repairResetEmbeddings opens the SAME vector backend the real
	// repair command would (this open triggers the one-time backfill).
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{}
	cfg.Data.DataDir = dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = vecPath
	cfg.Vector.Embeddings.Dimension = 4

	// Drive the real repair embed-fixup flow on message 1: open backend (runs +
	// marks the backfill) → ResetEmbedGen([1]) → lower watermark → close.
	require.NoError(t, repairResetEmbeddings(ctx, s, []int64{1}),
		"repairResetEmbeddings")

	// FIX A assertion: the repaired message must end embed_gen IS NULL — the
	// re-embed request survives. With the OLD reset-before-open ordering, the
	// backfill (running on the open that lowers the watermark) would re-stamp
	// it back to =active here.
	_, isNull1 := readEmbedGen(t, s.DB(), 1)
	assert.True(t, isNull1,
		"repaired message 1 must end embed_gen=NULL so scan-and-fill re-embeds it")

	// Sanity: message 2 was NOT repaired, so the backfill legitimately stamps it
	// back to the active generation (it stays "covered").
	v2, isNull2 := readEmbedGen(t, s.DB(), 2)
	assert.False(t, isNull2, "unrepaired message 2 must be re-stamped by the backfill")
	assert.Equal(t, int64(gen), v2, "message 2 embed_gen = active generation")
}

// TestRepairResetEmbeddings_VectorDisabledStillResets pins the !Enabled branch:
// with vector search disabled, repairResetEmbeddings opens no backend (no
// backfill) but still clears embed_gen so the column is consistent. No error.
func TestRepairResetEmbeddings_VectorDisabledStillResets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")

	s, err := store.Open(mainPath)
	require.NoError(t, err, "store.Open")
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.InitSchema(), "InitSchema")
	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, embed_gen)
VALUES (1, 1, 1, 'm1', 'email', 7);
`)
	require.NoError(t, err, "seed message with embed_gen set")

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{}
	cfg.Data.DataDir = dir
	cfg.Vector.Enabled = false

	require.NoError(t, repairResetEmbeddings(ctx, s, []int64{1}),
		"repairResetEmbeddings (vector disabled)")

	_, isNull := readEmbedGen(t, s.DB(), 1)
	assert.True(t, isNull, "embed_gen must be cleared even when vector search is disabled")
}
