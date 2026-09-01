package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
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

	template, err := sqliteTemplateBytes()
	require.NoError(t, err, "build sqlite template")

	dbPath := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, os.WriteFile(dbPath, template, 0o600), "clone sqlite template")

	st, err := store.OpenForTest(dbPath)
	require.NoError(t, err, "open store")

	t.Cleanup(func() {
		_ = st.Close()
	})
	assignFreshArchiveUID(t, st)

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

// newPostgresTestStore creates a test-isolated PostgreSQL store: a clone of
// the binary's template database, or — when the server cannot build one, or
// cloning is switched off — a private schema in the configured database,
// created and migrated here exactly as before templates existed. Either way
// the database is private to this test, never previously used, and dropped on
// cleanup.
func newPostgresTestStore(t *testing.T, dbURL string) *store.Store {
	t.Helper()

	if os.Getenv(templateDisableEnv) != "0" {
		if tmpl := templateFor(dbURL); tmpl.ensure() == nil {
			return cloneTestDatabase(t, tmpl)
		}
	}

	return newSchemaTestStore(t, dbURL)
}

// cloneTestDatabase hands the test a fresh clone of the template.
func cloneTestDatabase(t *testing.T, tmpl *pgTemplate) *store.Store {
	t.Helper()

	name, err := tmpl.clone(context.Background())
	require.NoError(t, err, "clone template database")

	// Register removal before opening, so a failure below does not leak the
	// clone.
	var st *store.Store
	t.Cleanup(func() {
		if st != nil {
			_ = st.Close()
		}
		if admin, err := pgAdminDB(tmpl.dbURL); err == nil {
			dropOwnedDatabase(admin, name)
		}
	})

	st, err = store.Open(withDatabase(tmpl.dbURL, name))
	require.NoError(t, err, "open store")
	assignFreshArchiveUID(t, st)

	return st
}

// newSchemaTestStore is the per-schema path: create a schema, migrate it, drop
// it on cleanup.
func newSchemaTestStore(t *testing.T, dbURL string) *store.Store {
	t.Helper()

	schemaName := createTestSchema(t, dbURL)

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
	require.NoError(t, st.InitSchema(), "init schema")

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

// fixtureSchemaNamePattern matches the schemas createTestSchema builds, so a
// drop can rebuild its target from validated parts.
var fixtureSchemaNamePattern = regexp.MustCompile("^msgvault_test_([0-9a-f]{16})$")

// dropOwnedSchema removes a schema this package created, and does nothing at
// all for any other name. Failure is ignored: the schema is a leak, not a
// wrong verdict.
func dropOwnedSchema(db *sql.DB, name string) {
	match := fixtureSchemaNamePattern.FindStringSubmatch(name)
	if match == nil {
		return
	}

	_, _ = db.Exec("DROP SCHEMA IF EXISTS msgvault_test_" + match[1] + " CASCADE")
}

// schemaURL returns dbURL with its search_path pointed at a schema.
func schemaURL(dbURL, schemaName string) string {
	separator := "?"
	if strings.Contains(dbURL, "?") {
		separator = "&"
	}

	return dbURL + separator + "search_path=" + schemaName
}

// assignFreshArchiveUID gives a cloned database its own archive identity.
// InitSchema() mints one durable UID per database and the template carries the
// one it was built with; a clone is a new archive, so it must not share it.
// The key is the store's own ("archive_uid", see store/archive_identity.go),
// written here because a store never reassigns an identity in production.
func assignFreshArchiveUID(t *testing.T, st *store.Store) {
	t.Helper()

	random := make([]byte, 32)
	_, err := rand.Read(random)
	require.NoError(t, err, "random archive UID")

	statement := "UPDATE archive_metadata SET value = ? WHERE key = 'archive_uid'"
	if st.IsPostgreSQL() {
		statement = "UPDATE archive_metadata SET value = $1 WHERE key = 'archive_uid'"
	}
	result, err := st.DB().Exec(statement, hex.EncodeToString(random))
	require.NoError(t, err, "assign archive UID")
	updated, err := result.RowsAffected()
	require.NoError(t, err, "assigned archive UID rows")
	require.Equal(t, int64(1), updated, "the template carried exactly one archive UID to replace")
}
