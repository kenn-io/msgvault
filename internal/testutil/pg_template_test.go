package testutil

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// requirePostgresTestURL returns the configured PostgreSQL test URL, skipping
// the calling test when MSGVAULT_TEST_DB is unset or points at SQLite.
func requirePostgresTestURL(t *testing.T) string {
	t.Helper()
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(testDB, "postgres://") && !strings.HasPrefix(testDB, "postgresql://") {
		t.Skip("PostgreSQL-only: set MSGVAULT_TEST_DB to a postgres:// URL")
	}
	return testDB
}

// requireTemplate returns this binary's template, skipping the calling test
// when fixtures are on the per-schema path instead: cloning switched off, or
// a role that cannot CREATE DATABASE. The contracts below are the template's.
func requireTemplate(t *testing.T, dbURL string) *pgTemplate {
	t.Helper()
	if os.Getenv(templateDisableEnv) == "0" {
		t.Skip("template cloning is switched off")
	}
	tmpl := templateFor(dbURL)
	if err := tmpl.ensure(); err != nil {
		t.Skipf("template cloning unavailable on this server: %v", err)
	}
	return tmpl
}

// currentSchemaOf reports the schema a store's connections resolve unqualified
// names against.
func currentSchemaOf(t *testing.T, st *store.Store) string {
	t.Helper()
	var name string
	require.NoError(t, st.DB().QueryRow("SELECT current_schema()").Scan(&name), "select current_schema")
	return name
}

// countSources reports how many rows the store's sources table holds. A fresh
// fixture has none; a fixture that shares a database with another test does
// not.
func countSources(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM sources").Scan(&count), "count sources")
	return count
}

// currentDatabaseOf reports the database a store's connections are attached
// to, which is the per-test clone the fixture handed it.
func currentDatabaseOf(t *testing.T, st *store.Store) string {
	t.Helper()
	var name string
	require.NoError(t, st.DB().QueryRow("SELECT current_database()").Scan(&name), "select current_database")
	return name
}

// databaseExists reports whether a database of the given name is present.
func databaseExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var count int
	require.NoError(t,
		db.QueryRow("SELECT count(*) FROM pg_database WHERE datname = $1", name).Scan(&count),
		"count pg_database rows")
	return count > 0
}

// TestPostgresFixturesGetPrivateEmptyDatabases locks the fixture contract:
// two fixtures in one binary get different databases, each starts empty, and
// writes through one are invisible to the other.
func TestPostgresFixturesGetPrivateEmptyDatabases(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requireTemplate(t, requirePostgresTestURL(t))

	first := NewTestStore(t)
	second := NewTestStore(t)

	assert.NotEqual(currentDatabaseOf(t, first), currentDatabaseOf(t, second), "fixtures must not share a database")
	assert.Equal(0, countSources(t, first), "first fixture starts empty")
	assert.Equal(0, countSources(t, second), "second fixture starts empty")

	_, err := first.GetOrCreateSource("gmail", "isolation@example.com")
	require.NoError(err, "write through the first fixture")

	assert.Equal(1, countSources(t, first), "write lands in the first fixture")
	assert.Equal(0, countSources(t, second), "write must not leak into the second fixture")
}

// TestPostgresFixtureDatabaseDroppedAfterCleanup proves the claiming test owns
// removal of the clone it was handed.
func TestPostgresFixtureDatabaseDroppedAfterCleanup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := requirePostgresTestURL(t)
	requireTemplate(t, dbURL)

	adminDB, err := pgAdminDB(dbURL)
	require.NoError(err, "open admin connection")

	var database string
	t.Run("owner", func(t *testing.T) {
		database = currentDatabaseOf(t, NewTestStore(t))
	})

	require.NotEmpty(database, "subtest recorded its database")
	assert.False(databaseExists(t, adminDB, database), "clone is gone after the owning test's cleanup")
}

// TestPostgresFixtureIssuesNoDDLOnceTemplateExists is the invariant that
// decides the fixture's shape. Several tests capture slog.Default() and assert
// over the whole buffer, so once the template is built no fixture may put
// schema statements on the wire — not in its own setup, and never while a test
// body is running.
func TestPostgresFixtureIssuesNoDDLOnceTemplateExists(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requireTemplate(t, requirePostgresTestURL(t))

	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true, MaxStmtChars: 10_000})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	var logged bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	fixture := NewTestStore(t)
	for i := range 20 {
		_, err := fixture.GetOrCreateSource("gmail", fmt.Sprintf("quiet-%d@example.test", i))
		require.NoError(err, "write through the fixture")
	}

	trace := strings.ToLower(logged.String())
	require.Contains(trace, "sources", "the capture observed this test's own queries")
	assert.NotContains(trace, "create table", "fixture setup must clone the template, not replay the schema")
	assert.NotContains(trace, "create index", "fixture setup must clone the template, not replay the schema")
}

// TestNewTestStoreFallsBackWhenTemplatesDisabled covers the path a server
// without CREATEDB takes: the fixture creates its own schema in the configured
// database and behaves exactly as before templates existed.
func TestNewTestStoreFallsBackWhenTemplatesDisabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requirePostgresTestURL(t)
	t.Setenv(templateDisableEnv, "0")

	st := NewTestStore(t)

	assert.Truef(strings.HasPrefix(currentSchemaOf(t, st), "msgvault_test_"),
		"the fallback path creates its own schema, got %q", currentSchemaOf(t, st))
	assert.Equal(0, countSources(t, st), "fallback fixture starts empty")

	_, err := st.GetOrCreateSource("gmail", "fallback@example.com")
	require.NoError(err, "write through the fallback fixture")
	assert.Equal(1, countSources(t, st), "fallback fixture is writable")
}

// createOwnedDatabaseForTest creates a database shaped like one of ours and
// registers its removal, so a test can stage databases for the sweep without
// leaking them on failure.
func createOwnedDatabaseForTest(t *testing.T, admin *sql.DB, name string) {
	t.Helper()
	_, err := admin.Exec("CREATE DATABASE " + name)
	require.NoErrorf(t, err, "create database %s", name)
	t.Cleanup(func() { dropOwnedDatabase(admin, name) })
}

// TestSweepReclaimsOnlyUnownedTemplates covers the sweep's ownership rule: a
// template whose owner session is gone is reclaimed together with its clones;
// one whose lock is still held — by another running binary, or by this one —
// is left alone; and one from another scope is never judged at all, because
// its owner's lock lives in a database this sweep cannot observe.
func TestSweepReclaimsOnlyUnownedTemplates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := requirePostgresTestURL(t)

	admin, err := pgAdminDB(dbURL)
	require.NoError(err, "open admin connection")
	ctx := context.Background()

	// This binary's own template first: building it sweeps, and the staged
	// databases below must be judged by the sweep under test, not that one.
	own := requireTemplate(t, dbURL)

	deadToken, err := newTemplateToken()
	require.NoError(err, "dead owner token")
	dead := templateDBPrefix + own.scope + "_" + deadToken
	deadClone := cloneDBPrefix + own.scope + "_" + deadToken + "_" + deadToken
	createOwnedDatabaseForTest(t, admin, dead)
	createOwnedDatabaseForTest(t, admin, deadClone)

	// A live owner elsewhere: its lock is held on a session that outlives the
	// sweep, exactly as a running sibling binary holds its own.
	liveToken, err := newTemplateToken()
	require.NoError(err, "live owner token")
	live := templateDBPrefix + own.scope + "_" + liveToken
	createOwnedDatabaseForTest(t, admin, live)

	// A template from a run configured against another database on this
	// server. Nobody holds its lock here, and that is exactly why the sweep
	// must not read anything into it.
	foreignScope := "0123abcd"
	if foreignScope == own.scope {
		foreignScope = "abcd0123"
	}
	foreignToken, err := newTemplateToken()
	require.NoError(err, "foreign owner token")
	foreign := templateDBPrefix + foreignScope + "_" + foreignToken
	createOwnedDatabaseForTest(t, admin, foreign)
	holder, err := admin.Conn(ctx)
	require.NoError(err, "pin holder session")
	_, err = holder.ExecContext(ctx, "SELECT pg_advisory_lock($1)", templateLockKey(liveToken))
	require.NoError(err, "hold the live owner's lock")
	t.Cleanup(func() {
		_, _ = holder.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", templateLockKey(liveToken))
		_ = holder.Close()
	})

	dropped, err := sweepOrphanTemplates(ctx, admin, own.scope)
	require.NoError(err, "sweep")

	// Other binaries' leftovers may be reclaimed in the same pass, so the
	// verdict is per database rather than over the whole list.
	assert.Contains(dropped, dead, "sweep reclaims the dead owner's template")
	assert.Contains(dropped, deadClone, "sweep reclaims the dead owner's clone")
	assert.False(databaseExists(t, admin, dead), "dead owner's template is gone")
	assert.False(databaseExists(t, admin, deadClone), "dead owner's clone is gone")
	assert.True(databaseExists(t, admin, live), "a live owner's template survives")
	assert.True(databaseExists(t, admin, own.name()), "this binary's own template survives")
	assert.NotContains(dropped, foreign, "sweep never judges another scope's template")
	assert.True(databaseExists(t, admin, foreign), "another scope's template survives")
}

// TestDropOwnedDatabaseIgnoresForeignNames is the safety gate on every DROP
// DATABASE this package issues: only names it generated can reach SQL.
func TestDropOwnedDatabaseIgnoresForeignNames(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := requirePostgresTestURL(t)

	admin, err := pgAdminDB(dbURL)
	require.NoError(err, "open admin connection")

	var configured string
	require.NoError(admin.QueryRow("SELECT current_database()").Scan(&configured), "configured database")
	// Not through createOwnedDatabaseForTest: its cleanup is the function under
	// test, which must refuse this name, so the test drops it directly.
	lookalike := templateDBPrefix + "lookalike"
	_, err = admin.Exec("DROP DATABASE IF EXISTS " + lookalike)
	require.NoError(err, "clear a leftover lookalike")
	_, err = admin.Exec("CREATE DATABASE " + lookalike)
	require.NoError(err, "create lookalike database")
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + lookalike) })

	dropOwnedDatabase(admin, configured)
	dropOwnedDatabase(admin, lookalike)

	assert.True(databaseExists(t, admin, configured), "the configured database can never be a target")
	assert.True(databaseExists(t, admin, lookalike), "a name outside the generated shape is never dropped")
}
