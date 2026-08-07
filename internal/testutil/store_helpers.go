package testutil

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// NewTestStore creates a temporary database for testing.
// The database is automatically cleaned up when the test completes.
//
// Backend selection via MSGVAULT_TEST_DB env var:
//   - unset or empty: SQLite (default)
//   - starts with "postgres://" or "postgresql://": PostgreSQL
//
// For PostgreSQL, each test gets its own schema (created and dropped for isolation).
func NewTestStore(t *testing.T) *store.Store {
	t.Helper()

	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if strings.HasPrefix(testDB, "postgres://") || strings.HasPrefix(testDB, "postgresql://") {
		return newPostgresTestStore(t, testDB)
	}

	return NewSQLiteTestStore(t)
}

// NewSQLiteTestStore creates a temporary SQLite store, ALWAYS, ignoring
// MSGVAULT_TEST_DB. Use it for tests that are intrinsically tied to a SQLite
// main DB regardless of the configured backend — e.g. the sqlitevec vectors
// backend, whose Open-time probes (mainTableExists' sqlite_master lookup,
// resetOrphanedEmbedGen/BackfillEmbedGenForUpgrade) run SQLite-dialect SQL
// against the main handle. In production sqlitevec is only ever paired with a
// SQLite main store (the backend factory picks pgvector when the store is
// PostgreSQL), so such a test must not adopt a PostgreSQL main store just
// because MSGVAULT_TEST_DB is set.
func NewSQLiteTestStore(t *testing.T) *store.Store {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(t, err, "open store")

	t.Cleanup(func() {
		_ = st.Close()
	})

	require.NoError(t, st.InitSchema(), "init schema")

	return st
}

// SkipIfPostgres skips the calling test when MSGVAULT_TEST_DB targets
// PostgreSQL. Use this for tests that exercise SQLite-only constructs
// (FTS5 MATCH, PRAGMA, BEGIN EXCLUSIVE, SQLite trigger syntax) where
// PostgreSQL's portable equivalent is covered by a separate test or
// by the Dialect interface.
func SkipIfPostgres(t *testing.T, reason string) {
	t.Helper()
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if strings.HasPrefix(testDB, "postgres://") || strings.HasPrefix(testDB, "postgresql://") {
		t.Skipf("skipping on PostgreSQL: %s", reason)
	}
}

// newPostgresTestStore creates a test-isolated PostgreSQL store using a random schema name.
// The schema is dropped on test cleanup.
//
// The schema is taken from the warm pool when one is ready — already created
// and already migrated, so the test pays only for opening its own connection.
// Otherwise the test creates and migrates one itself, exactly as it always did.
// Either way the schema is private to this test, never previously used, and
// dropped on cleanup.
func newPostgresTestStore(t *testing.T, dbURL string) *store.Store {
	t.Helper()

	schemaName, warmed := claimWarmSchema(dbURL)
	if !warmed {
		schemaName = createTestSchema(t, dbURL)
	}

	// Register schema cleanup immediately so that any failure below this
	// point (store.Open, InitSchema) doesn't leak the schema.
	var st *store.Store
	t.Cleanup(func() {
		if st != nil {
			_ = st.Close()
		}
		cleanupDB, err := pgAdminDB(dbURL)
		if err != nil {
			return
		}
		dropOwnedSchema(cleanupDB, schemaName)
	})

	st, err := store.Open(schemaURL(dbURL, schemaName))
	require.NoError(t, err, "open store")
	if !warmed {
		require.NoError(t, st.InitSchema(), "init schema")
	}

	return st
}

// createTestSchema creates an empty, unmigrated schema for one test. This is
// the path a fixture takes when the warm pool has nothing ready or is
// unavailable, so its failures must read exactly as they did before the pool
// existed.
func createTestSchema(t *testing.T, dbURL string) string {
	t.Helper()

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema name")
	schemaName := "msgvault_test_" + hex.EncodeToString(buf)

	setupDB, err := pgAdminDB(dbURL)
	require.NoError(t, err, "open setup connection")
	_, schemaErr := setupDB.Exec("CREATE SCHEMA " + schemaName)
	require.NoErrorf(t, schemaErr, "create schema %s", schemaName)

	return schemaName
}
