package testutil

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx driver for test setup
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

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.OpenForTest(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() {
		_ = st.Close()
	})

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	return st
}

// newPostgresTestStore creates a test-isolated PostgreSQL store using a random schema name.
// The schema is dropped on test cleanup.
func newPostgresTestStore(t *testing.T, dbURL string) *store.Store {
	t.Helper()

	// Generate a random schema name for test isolation
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random schema name: %v", err)
	}
	schemaName := "msgvault_test_" + hex.EncodeToString(buf)

	// Create the schema using a separate connection
	setupDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("open setup connection: %v", err)
	}
	if _, err := setupDB.Exec(fmt.Sprintf("CREATE SCHEMA %s", schemaName)); err != nil {
		_ = setupDB.Close()
		t.Fatalf("create schema %s: %v", schemaName, err)
	}
	_ = setupDB.Close()

	// Register schema cleanup immediately so that any failure below this
	// point (store.Open, InitSchema) doesn't leak the schema.
	var st *store.Store
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

	// Build a URL that uses the test schema via search_path
	testURL := dbURL
	sep := "?"
	if strings.Contains(dbURL, "?") {
		sep = "&"
	}
	testURL += sep + "search_path=" + schemaName

	st, err = store.Open(testURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	return st
}
