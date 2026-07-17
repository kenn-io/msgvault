package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipUnlessPostgresInternal skips the calling internal (package store) test
// unless MSGVAULT_TEST_DB points at PostgreSQL. The maintenance escape hatch
// (SET LOCAL statement_timeout = 0) and the cascade-lock invariant are
// PostgreSQL-only: SQLite has no statement_timeout and no LOCK TABLE.
func skipUnlessPostgresInternal(t *testing.T) string {
	t.Helper()
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("PG-only: maintenance timeout hatch / cascade lock invariant; requires MSGVAULT_TEST_DB pointing at PostgreSQL")
	}
	return testDB
}

// newPGStoreInternal opens a schema-isolated PostgreSQL store for an internal
// (package store) test. It mirrors testutil.newPostgresTestStore but lives in
// package store so the test can reach unexported symbols (exclusiveLockTables,
// runMaintenance). The schema is dropped on cleanup.
func newPGStoreInternal(t *testing.T, dbURL string) *Store {
	t.Helper()

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "msgvault_test_" + hex.EncodeToString(buf)

	setupDB, err := sql.Open("pgx", dbURL)
	require.NoError(t, err, "open setup connection")
	_, schemaErr := setupDB.Exec("CREATE SCHEMA " + schemaName)
	_ = setupDB.Close()
	require.NoErrorf(t, schemaErr, "create schema %s", schemaName)

	var st *Store
	t.Cleanup(func() {
		if st != nil {
			_ = st.Close()
		}
		cleanupDB, err := sql.Open("pgx", dbURL)
		if err != nil {
			return
		}
		defer func() { _ = cleanupDB.Close() }()
		_, _ = cleanupDB.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE", schemaName))
	})

	sep := "?"
	if strings.Contains(dbURL, "?") {
		sep = "&"
	}
	testURL := dbURL + sep + "search_path=" + schemaName

	st, err = Open(testURL)
	require.NoError(t, err, "open store")
	require.NoError(t, st.InitSchema(), "init schema")
	return st
}

// TestExclusiveLockTablesCoverCascade pins finding S4: every table with a
// direct ON DELETE CASCADE foreign key to sources(id) MUST appear in
// exclusiveLockTables, otherwise RemoveSourceSerialized's cascade DELETE can
// race a concurrent writer to that table and reopen the race the EXCLUSIVE
// lock exists to close.
//
// Before the fix, source_import_items (written by UpsertSourceImportItem) and
// sync_checkpoints were absent, so this test would fail and name them. It is a
// single-level pg_constraint query because both newly-added tables are direct
// FKs to sources; that is authoritative for the cascade tables this lock must
// cover.
func TestExclusiveLockTablesCoverCascade(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t, dbURL)

	// Tables with a direct ON DELETE CASCADE FK to sources(id), read from the
	// catalog (scoped to the test's current schema so sibling schemas can't
	// leak in).
	rows, err := st.DB().Query(`
		SELECT DISTINCT c.conrelid::regclass::text
		FROM pg_constraint c
		JOIN pg_class child ON child.oid = c.conrelid
		JOIN pg_namespace ns ON ns.oid = child.relnamespace
		WHERE c.contype = 'f'
		  AND c.confrelid = 'sources'::regclass
		  AND c.confdeltype = 'c'
		  AND ns.nspname = current_schema()
	`)
	require.NoError(err, "query cascade tables")
	defer func() { _ = rows.Close() }()

	lockSet := make(map[string]bool, len(exclusiveLockTables))
	for _, tbl := range exclusiveLockTables {
		lockSet[tbl] = true
	}

	var cascadeTables []string
	for rows.Next() {
		var name string
		require.NoError(rows.Scan(&name), "scan cascade table name")
		// conrelid::regclass may schema-qualify; take the bare table name.
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		name = strings.Trim(name, `"`)
		cascadeTables = append(cascadeTables, name)
	}
	require.NoError(rows.Err(), "iterate cascade tables")

	// Sanity: the catalog must actually report cascade tables, otherwise the
	// query is wrong and the test would vacuously pass.
	require.NotEmpty(cascadeTables, "expected cascade-to-sources tables in catalog")
	assert.Contains(cascadeTables, "source_import_items",
		"source_import_items must be a direct cascade target (sanity)")
	assert.Contains(cascadeTables, "sync_checkpoints",
		"sync_checkpoints must be a direct cascade target (sanity)")

	var missing []string
	for _, tbl := range cascadeTables {
		if !lockSet[tbl] {
			missing = append(missing, tbl)
		}
	}
	sort.Strings(missing)
	assert.Empty(missing,
		"every ON DELETE CASCADE-to-sources table must be in exclusiveLockTables; missing: %v", missing)
}

// TestMaintenanceTimeoutResetSQL pins the exact statement the PG dialect uses
// to lift the per-statement timeout — the mechanism finding S1's hatch relies
// on. SQLite returns "" so runMaintenance issues no reset.
func TestMaintenanceTimeoutResetSQL(t *testing.T) {
	assert.Equal(t, "SET LOCAL statement_timeout = 0", (&PostgreSQLDialect{}).MaintenanceTimeoutResetSQL())
	assert.Empty(t, (&SQLiteDialect{}).MaintenanceTimeoutResetSQL())
}

// is57014 reports whether err is the PostgreSQL query_canceled SQLSTATE raised
// when statement_timeout fires.
func is57014(err error) bool { return isPgError(err, "57014") }

// TestMaintenanceHatchLiftsStatementTimeout proves finding S1's hatch actually
// disables the per-statement timeout on PostgreSQL using a deterministic
// pg_sleep that outlasts a low timeout.
//
//   - Negative control: under SET LOCAL statement_timeout='100ms',
//     SELECT pg_sleep(0.3) is cancelled with SQLSTATE 57014.
//   - Positive (exact reset SQL): issuing the dialect's
//     MaintenanceTimeoutResetSQL (SET LOCAL statement_timeout = 0) after the
//     low SET LOCAL lets the same pg_sleep(0.3) SUCCEED — all on one tx/conn,
//     which is required for SET LOCAL to take effect.
//   - End-to-end: runMaintenance over a single-connection pool whose session
//     statement_timeout is 100ms runs pg_sleep(0.3) to completion, because the
//     hatch resets the timeout to 0 inside the maintenance tx.
func TestMaintenanceHatchLiftsStatementTimeout(t *testing.T) {
	dbURL := skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t, dbURL)
	ctx := context.Background()

	d := &PostgreSQLDialect{}

	// Negative control: a low SET LOCAL timeout cancels the long sleep.
	t.Run("low_timeout_cancels_without_reset", func(t *testing.T) {
		require := require.New(t)
		tx, err := st.DB().BeginTx(ctx, nil)
		require.NoError(err, "begin tx")
		defer func() { _ = tx.Rollback() }()

		_, err = tx.ExecContext(ctx, "SET LOCAL statement_timeout = '100ms'")
		require.NoError(err, "set low statement_timeout")

		_, err = tx.ExecContext(ctx, "SELECT pg_sleep(0.3)")
		require.Error(err, "pg_sleep(0.3) must be cancelled under a 100ms timeout")
		assert.True(t, is57014(err), "expected SQLSTATE 57014 (query_canceled), got %v", err)
	})

	// Positive: the dialect's exact reset SQL lifts the low timeout in-tx.
	t.Run("reset_sql_lifts_low_timeout", func(t *testing.T) {
		require := require.New(t)
		tx, err := st.DB().BeginTx(ctx, nil)
		require.NoError(err, "begin tx")
		defer func() { _ = tx.Rollback() }()

		_, err = tx.ExecContext(ctx, "SET LOCAL statement_timeout = '100ms'")
		require.NoError(err, "set low statement_timeout")

		_, err = tx.ExecContext(ctx, d.MaintenanceTimeoutResetSQL())
		require.NoError(err, "apply maintenance reset SQL")

		_, err = tx.ExecContext(ctx, "SELECT pg_sleep(0.3)")
		require.NoError(err, "pg_sleep(0.3) must succeed once the timeout is reset to 0")
	})

	// End-to-end: runMaintenance over a 1-connection pool whose session
	// statement_timeout is low. The single physical connection retains the
	// session GUC across Close, so runMaintenance reuses it; the hatch's
	// SET LOCAL statement_timeout = 0 must override it for the maintenance tx.
	t.Run("runMaintenance_overrides_session_timeout", func(t *testing.T) {
		require := require.New(t)
		st.DB().SetMaxOpenConns(1)
		st.DB().SetMaxIdleConns(1)

		// Pin the session timeout on the single pooled connection.
		conn, err := st.DB().Conn(ctx)
		require.NoError(err, "grab the single pooled connection")
		_, err = conn.ExecContext(ctx, "SET statement_timeout = '100ms'")
		require.NoError(err, "set session statement_timeout low")
		require.NoError(conn.Close(), "return connection to pool")

		// Confirm the session timeout actually bites without the hatch: a bare
		// pg_sleep(0.3) on the pool must be cancelled.
		_, err = st.DB().ExecContext(ctx, "SELECT pg_sleep(0.3)")
		require.Error(err, "bare pg_sleep(0.3) must be cancelled by the 100ms session timeout")
		require.True(is57014(err), "expected 57014 from bare pg_sleep, got %v", err)

		// Through the hatch: the same long sleep must complete.
		err = st.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			_, err := tx.ExecContext(ctx, "SELECT pg_sleep(0.3)")
			return err
		})
		require.NoError(err, "runMaintenance must lift the session timeout and let pg_sleep(0.3) complete")
	})
}

// TestRepackMetadataMaintenanceLiftsStatementTimeout proves the public repack
// metadata phases use runMaintenance rather than the pool's ordinary timeout.
// Slow DELETE triggers make both stale-index repair and zero-live record
// cleanup exceed a deliberately tiny session timeout; both must still finish.
func TestRepackMetadataMaintenanceLiftsStatementTimeout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t, dbURL)
	ctx := context.Background()
	const (
		packID = "01hzy3v7q8r9s0t1a2v3w4x6j1"
		hash   = "aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee"
	)

	_, err := st.DB().Exec(`
		INSERT INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
		VALUES ($1, 1, 64, $2)`, packID, time.Now().UTC().Format(time.RFC3339))
	require.NoError(err, "insert zero-live pack record")
	_, err = st.DB().Exec(`
		INSERT INTO attachment_pack_index
		    (blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
		VALUES ($1, $2, 6, 64, 64, 0, 0)`, hash, packID)
	require.NoError(err, "insert stale mapping")
	_, err = st.DB().Exec(`
		CREATE FUNCTION slow_repack_maintenance_delete() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
		    PERFORM pg_sleep(0.3);
		    RETURN OLD;
		END $$`)
	require.NoError(err, "create slow maintenance trigger function")
	_, err = st.DB().Exec(`
		CREATE TRIGGER slow_repack_mapping_delete
		BEFORE DELETE ON attachment_pack_index
		FOR EACH ROW EXECUTE FUNCTION slow_repack_maintenance_delete()`)
	require.NoError(err, "create slow mapping trigger")
	_, err = st.DB().Exec(`
		CREATE TRIGGER slow_repack_record_delete
		BEFORE DELETE ON attachment_packs
		FOR EACH ROW EXECUTE FUNCTION slow_repack_maintenance_delete()`)
	require.NoError(err, "create slow pack-record trigger")

	st.DB().SetMaxOpenConns(1)
	st.DB().SetMaxIdleConns(1)
	conn, err := st.DB().Conn(ctx)
	require.NoError(err, "grab single pooled connection")
	_, err = conn.ExecContext(ctx, "SET statement_timeout = '100ms'")
	require.NoError(err, "set short session timeout")
	require.NoError(conn.Close(), "return connection to pool")
	_, err = st.DB().ExecContext(ctx, "SELECT pg_sleep(0.3)")
	require.Error(err, "negative control must hit the session timeout")
	assert.True(is57014(err), "expected SQLSTATE 57014 from negative control, got %v", err)

	pruned, err := st.PruneUnreferencedPackIndex(ctx)
	require.NoError(err, "slow prune must run with maintenance timeout disabled")
	assert.Equal(int64(1), pruned)
	usage, err := st.ListPackUsage(ctx)
	require.NoError(err, "usage accounting shares the context-aware maintenance path")
	require.Len(usage, 1)
	assert.Zero(usage[0].LiveEntries)
	assert.Zero(usage[0].MaxLiveStoredLen)
	assert.Zero(usage[0].MaxLiveRawLen)
	entries, err := st.ListReferencedPackEntries(ctx, packID)
	require.NoError(err, "referenced enumeration shares the maintenance path")
	assert.Empty(entries)
	deleted, err := st.DeleteEmptyPackRecord(ctx, packID)
	require.NoError(err, "slow cleanup must run with maintenance timeout disabled")
	assert.True(deleted)
}
