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
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// makeUpgradedMainDB writes a real msgvault.db and then DROPs the embed_gen
// column from messages, reproducing the exact shape a v0.14/v0.15 archive
// has after upgrading to a binary that introduced embed_gen but before any
// InitSchema has run against it: a full messages table that is missing only
// embed_gen. Any read/write of embed_gen fails with "no such column".
//
// It seeds one live message so coverage/backfill have something to act on.
func makeUpgradedMainDB(t *testing.T, mainPath string) {
	t.Helper()
	s, err := store.Open(mainPath)
	require.NoError(t, err, "store.Open for fixture")
	require.NoError(t, s.InitSchema(), "InitSchema for fixture")
	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type)
VALUES (1, 1, 1, 'm1', 'email');
`)
	require.NoError(t, err, "seed message")
	// Drop the backfill ledger row (none expected yet) and the embed_gen
	// column so the DB looks like a pre-embed_gen upgrade.
	_, err = s.DB().Exec(`ALTER TABLE messages DROP COLUMN embed_gen`)
	require.NoError(t, err, "drop embed_gen to simulate pre-upgrade schema")
	require.NoError(t, s.Close(), "close fixture store")
}

// dropEmbedGenColumn removes the embed_gen column from an existing main DB,
// simulating the post-seed pre-upgrade shape used by the failure-mode arm of
// the test. Kept separate from makeUpgradedMainDB so the seed (which needs a
// real active generation) can run while embed_gen still exists, then drop it
// just before the embed_gen-touching reopen.
func dropEmbedGenColumn(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`ALTER TABLE messages DROP COLUMN embed_gen`)
	require.NoError(t, err, "drop embed_gen to simulate pre-upgrade schema")
}

// TestEmbed_UpgradedDBMissingEmbedGen_NeedsInitSchema is the regression
// guard for Codex #2: the embeddings build/resume path (runEmbed) opened
// the store but never called InitSchema, so on an upgraded DB whose
// messages table lacked embed_gen the upgrade backfill and CoverageCounts
// failed with "no such column: embed_gen" before InitSchema would have
// added it.
//
// The test first asserts the failure mode is real: against an upgraded DB
// with an active generation but the column-less messages table, opening the
// backend (which runs BackfillEmbedGenForUpgrade) errors on the missing
// column. Then it asserts the fix: running the runEmbed ordering
// (store.Open -> s.InitSchema() -> sqlitevec.Open -> s.CoverageCounts)
// succeeds because InitSchema adds embed_gen before any embed_gen access.
func TestEmbed_UpgradedDBMissingEmbedGen_NeedsInitSchema(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	require.NoError(
		sqlitevec.RegisterExtension(), "RegisterExtension")

	ctx := context.Background()

	// --- 1. Failure mode without InitSchema -------------------------------
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	vecPath := filepath.Join(dir, "vectors.db")
	// Seed with embed_gen still PRESENT: store.Open + InitSchema leave the
	// column in place. The seed backend Open below (which runs the orphaned-
	// stamp reset + the upgrade backfill, both of which touch embed_gen) must
	// succeed here — we drop the column only afterwards to reproduce the
	// upgraded-but-not-reinitialized shape for the failing reopen.
	sSeed, err := store.Open(mainPath)
	require.NoError(
		err, "store.Open for fixture (column present)")

	require.NoError(
		sSeed.InitSchema(), "InitSchema for fixture")

	_, err = sSeed.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type)
VALUES (1, 1, 1, 'm1', 'email');
`)
	require.NoError(
		err, "seed message")

	mainRaw := sSeed.DB()

	// Seed an active generation that already embedded msg 1, then clear the
	// backfill ledger so the next Open runs the real embed_gen-stamping
	// backfill. embed_gen is present here, so the reset + backfill succeed.
	seed, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path: vecPath, MainPath: mainPath, Dimension: 4, MainDB: mainRaw,
	})
	require.NoError(
		err, "seed backend Open")

	gen, err := seed.CreateGeneration(ctx, "model", 4, "model:4")
	require.NoError(
		err, "seed CreateGeneration")

	require.NoError(
		seed.Upsert(ctx, gen, []vector.Chunk{{
			MessageID: 1, ChunkIndex: 0, Vector: []float32{0, 0, 0, 1},
		}}), "seed Upsert")

	require.NoError(
		seed.ActivateGeneration(ctx, gen, true), "seed Activate")

	require.NoError(
		seed.Close(), "close seed backend")

	_, err = mainRaw.Exec(`DELETE FROM applied_migrations WHERE name = 'embed_gen_backfill_active_v1'`)
	require.NoError(
		err, "clear backfill ledger")

	// Now reproduce the pre-upgrade shape: drop embed_gen. Msg 1 is stamped
	// for the active gen, but the column no longer exists.
	dropEmbedGenColumn(t, mainRaw)

	// Reopen: the orphaned-stamp reset (which runs first) tries to read/clear
	// messages.embed_gen, which does not exist on this upgraded schema →
	// "no such column: embed_gen". (Were the reset to somehow pass, the
	// backfill's embed_gen stamp would fail identically.) Either way the Open
	// fails until runEmbed's InitSchema adds the column back.
	reopen, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path: vecPath, MainPath: mainPath, Dimension: 4, MainDB: mainRaw,
	})
	if err == nil {
		_ = reopen.Close()
	}
	require.Error(err, "open must fail on a messages table lacking embed_gen")
	assert.Contains(err.Error(), "embed_gen", "failure should be the missing embed_gen column")
	require.NoError(
		sSeed.Close(), "close seed store")

	// --- 2. The fix: InitSchema first, then the embed path succeeds --------
	dir3 := t.TempDir()
	mainPath3 := filepath.Join(dir3, "msgvault.db")
	makeUpgradedMainDB(t, mainPath3)

	// This is what runEmbed now does: Open + InitSchema BEFORE touching the
	// vector backend / embed_gen.
	s, err := store.Open(mainPath3)
	require.NoError(
		err, "store.Open")

	defer func() { _ = s.Close() }()
	require.NoError(
		s.InitSchema(), "InitSchema must add the embed_gen column")

	// Backend Open now runs BackfillEmbedGenForUpgrade against a schema that
	// HAS embed_gen — no error.
	b3, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      filepath.Join(dir3, "vectors.db"),
		MainPath:  mainPath3,
		Dimension: 4,
		MainDB:    s.DB(),
	})
	require.NoError(
		err, "backend Open after InitSchema must succeed")

	defer func() { _ = b3.Close() }()

	// CoverageCounts (the other embed_gen reader in runEmbed) also succeeds.
	live, _, _, missing, err := s.CoverageCounts(ctx, 1)
	require.NoError(
		err, "CoverageCounts after InitSchema must succeed")

	assert.Equal(int64(1), live, "one live message")
	assert.Equal(int64(1), missing, "unstamped message reads as missing for gen 1")
}
