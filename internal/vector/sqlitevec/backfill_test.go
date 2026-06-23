//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

// embedGenOf reads embed_gen for a message, reporting the value and whether
// it is NULL.
func embedGenOf(t *testing.T, db *sql.DB, id int64) (val int64, isNull bool) {
	t.Helper()
	var v sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT embed_gen FROM messages WHERE id = ?`, id).Scan(&v))
	return v.Int64, !v.Valid
}

// seedEmbeddedMain builds a real main DB with one live message, opens a
// writable backend, creates+activates a generation with an embedding for
// message 1, then simulates the pre-upgrade state: embed_gen NULL and the
// backfill ledger cleared. It returns the closed paths and the (still open)
// rw store so the caller can drive a fresh Open. The rw store/backend are
// closed before returning so a subsequent Open holds the only handles.
func seedEmbeddedMain(ctx context.Context, t *testing.T) (mainPath, vecPath string) {
	t.Helper()
	dir := t.TempDir()
	mainPath = filepath.Join(dir, "msgvault.db")
	vecPath = filepath.Join(dir, "vectors.db")

	s, err := store.Open(mainPath)
	require.NoError(t, err, "store.Open (rw)")
	require.NoError(t, s.InitSchema(), "InitSchema")
	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type)
VALUES (1, 1, 1, 'm1', 'email');
`)
	require.NoError(t, err, "seed message")

	rw, err := Open(ctx, Options{
		Path: vecPath, MainPath: mainPath, Dimension: 4, MainDB: s.DB(),
	})
	require.NoError(t, err, "rw backend Open")
	gen, err := rw.CreateGeneration(ctx, "model", 4, "model:4")
	require.NoError(t, err, "CreateGeneration")
	require.NoError(t, rw.Upsert(ctx, gen, vectorChunkOne()), "Upsert")
	require.NoError(t, rw.ActivateGeneration(ctx, gen, true), "Activate")
	require.NoError(t, rw.Close(), "close rw backend")

	// Simulate the pre-upgrade state: embed_gen NULL, ledger cleared. A
	// write-path Open would now stamp embed_gen=active and mark the ledger.
	_, err = s.DB().Exec(`UPDATE messages SET embed_gen = NULL`)
	require.NoError(t, err, "reset embed_gen")
	_, err = s.DB().Exec(`DELETE FROM applied_migrations WHERE name = ?`, embedGenBackfillMigration)
	require.NoError(t, err, "clear ledger")
	require.NoError(t, s.Close(), "close rw store")

	return mainPath, vecPath
}

// TestBackfillEmbedGen_StampAndMarkAtomic_RollbackOnMarkFailure is the
// regression guard for Codex 129d #2/#3: the embed_gen stamp UPDATE and the
// applied_migrations ledger mark must be ONE atomic transaction.
//
// Fault injection: a BEFORE INSERT trigger on applied_migrations RAISE(ABORT)s
// when the backfill ledger row is inserted. RAISE(ABORT) errors even under
// INSERT OR IGNORE, so the ledger-mark step fails AFTER the embed_gen UPDATE
// has run inside the same tx. If the two are atomic, the UPDATE must be ROLLED
// BACK (embed_gen stays NULL) and the ledger must stay UNMARKED — leaving the
// DB exactly pre-backfill so a later clean Open re-runs and completes.
func TestBackfillEmbedGen_StampAndMarkAtomic_RollbackOnMarkFailure(t *testing.T) {
	require.NoError(t, RegisterExtension(), "RegisterExtension")
	ctx := context.Background()
	mainPath, vecPath := seedEmbeddedMain(ctx, t)

	// Reopen the main DB read-write and install a fault that makes ONLY the
	// ledger mark fail. The embed_gen UPDATE on messages still succeeds, so a
	// non-atomic implementation would leave embed_gen stamped while the ledger
	// stays unmarked.
	s, err := store.Open(mainPath)
	require.NoError(t, err, "reopen main rw")
	defer func() { _ = s.Close() }()
	_, err = s.DB().Exec(`CREATE TRIGGER zz_fail_backfill_mark
		BEFORE INSERT ON applied_migrations
		WHEN NEW.name = '` + embedGenBackfillMigration + `'
		BEGIN SELECT RAISE(ABORT, 'injected backfill mark failure'); END;`)
	require.NoError(t, err, "install fault trigger")

	b, err := Open(ctx, Options{
		Path: vecPath, MainPath: mainPath, Dimension: 4, MainDB: s.DB(),
	})
	// Open runs the backfill; the injected mark failure must surface as an
	// error from Open (the tx rolls back).
	if b != nil {
		t.Cleanup(func() { _ = b.Close() })
	}
	require.Error(t, err, "Open must surface the injected ledger-mark failure")
	assert.Contains(t, err.Error(), "injected backfill mark failure")

	// Atomicity: the embed_gen stamp must have been ROLLED BACK with the
	// failed mark — stamp NOT applied, ledger NOT marked.
	_, isNull := embedGenOf(t, s.DB(), 1)
	assert.True(t, isNull,
		"embed_gen must be rolled back to NULL when the ledger mark fails (atomic)")
	assert.False(t, backfillLedgerMarked(t, s.DB()),
		"ledger must stay unmarked when the backfill tx rolls back")

	// Recovery: remove the fault and re-Open. The migration was never marked,
	// so the one-time backfill re-runs cleanly and now completes — both the
	// stamp and the mark land.
	_, err = s.DB().Exec(`DROP TRIGGER zz_fail_backfill_mark`)
	require.NoError(t, err, "drop fault trigger")

	b2, err := Open(ctx, Options{
		Path: vecPath, MainPath: mainPath, Dimension: 4, MainDB: s.DB(),
	})
	require.NoError(t, err, "clean re-Open must succeed (backfill re-runs)")
	t.Cleanup(func() { _ = b2.Close() })

	_, isNull = embedGenOf(t, s.DB(), 1)
	assert.False(t, isNull, "embed_gen stamped after clean re-Open")
	assert.True(t, backfillLedgerMarked(t, s.DB()),
		"ledger marked after clean re-Open")
}

// TestBackfillEmbedGen_StampAndMarkAtomic_BothPresentOnSuccess is the positive
// companion: a successful backfill leaves BOTH the embed_gen stamp and the
// ledger mark present (the all-or-nothing tx committed both).
func TestBackfillEmbedGen_StampAndMarkAtomic_BothPresentOnSuccess(t *testing.T) {
	require.NoError(t, RegisterExtension(), "RegisterExtension")
	ctx := context.Background()
	mainPath, vecPath := seedEmbeddedMain(ctx, t)

	s, err := store.Open(mainPath)
	require.NoError(t, err, "reopen main rw")
	defer func() { _ = s.Close() }()

	b, err := Open(ctx, Options{
		Path: vecPath, MainPath: mainPath, Dimension: 4, MainDB: s.DB(),
	})
	require.NoError(t, err, "Open (runs backfill)")
	t.Cleanup(func() { _ = b.Close() })

	// Both committed together.
	v, isNull := embedGenOf(t, s.DB(), 1)
	require.False(t, isNull, "embed_gen stamped after successful backfill")
	assert.Positive(t, v, "embed_gen references the active generation")
	assert.True(t, backfillLedgerMarked(t, s.DB()),
		"ledger marked after successful backfill")
}
