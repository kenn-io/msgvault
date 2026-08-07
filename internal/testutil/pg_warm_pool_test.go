package testutil

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
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

// currentSchemaOf reports the schema a store's connections resolve unqualified
// names against, which is the per-test schema the fixture handed it.
func currentSchemaOf(t *testing.T, st *store.Store) string {
	t.Helper()

	var name string
	require.NoError(t, st.DB().QueryRow("SELECT current_schema()").Scan(&name), "select current_schema")

	return name
}

// schemaExists reports whether a schema of the given name is present.
func schemaExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var count int
	require.NoError(t,
		db.QueryRow("SELECT count(*) FROM pg_namespace WHERE nspname = $1", name).Scan(&count),
		"count pg_namespace rows")

	return count > 0
}

// countSources reports how many rows the store's sources table holds. A fresh
// fixture has none; a fixture that shares a schema with another test does not.
func countSources(t *testing.T, st *store.Store) int {
	t.Helper()

	var count int
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM sources").Scan(&count), "count sources")

	return count
}

// randomSchemaSuffix returns a suffix in the same shape the fixture uses.
func randomSchemaSuffix(t *testing.T) string {
	t.Helper()

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random schema suffix")

	return hex.EncodeToString(buf)
}

// syntheticOwner returns an owner token in the shape a name carries, unique to
// the calling test. Posing the sweeper's question with one of these rather than
// this process's real token keeps a test independent of whether the host can
// identify its own pid namespace, and keeps it from competing with a sibling
// run of this binary for the same schemas.
func syntheticOwner(t *testing.T) string {
	t.Helper()

	buf := make([]byte, warmOwnerTokenBytes)
	_, err := rand.Read(buf)
	require.NoError(t, err, "random owner token")

	return hex.EncodeToString(buf)
}

// createSchemaForTest creates a schema and registers its removal, so a test can
// stage schemas for the sweeper to consider without leaking them on failure.
func createSchemaForTest(t *testing.T, db *sql.DB, name string) {
	t.Helper()

	_, err := db.Exec("CREATE SCHEMA " + name)
	require.NoErrorf(t, err, "create schema %s", name)
	t.Cleanup(func() {
		_, _ = db.Exec("DROP SCHEMA IF EXISTS " + name + " CASCADE")
	})
}

// reapedPID returns the pid of a process that has run to completion and been
// waited for, so the kernel no longer lists it under /proc.
func reapedPID(t *testing.T) int {
	t.Helper()

	cmd := exec.Command("/bin/true")
	require.NoError(t, cmd.Run(), "run short-lived helper process")
	pid := cmd.Process.Pid

	_, statErr := os.Stat("/proc/" + strconv.Itoa(pid))
	require.True(t, os.IsNotExist(statErr), "reaped helper pid must be absent from /proc")

	return pid
}

// TestPostgresFixturesGetPrivateEmptySchemas locks the fixture contract the
// warm pool must not weaken: two fixtures in one binary get different schemas,
// each starts empty, and writes through one are invisible to the other.
func TestPostgresFixturesGetPrivateEmptySchemas(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requirePostgresTestURL(t)

	first := NewTestStore(t)
	second := NewTestStore(t)

	firstSchema := currentSchemaOf(t, first)
	secondSchema := currentSchemaOf(t, second)
	assert.NotEqual(firstSchema, secondSchema, "fixtures must not share a schema")

	assert.Equal(0, countSources(t, first), "first fixture starts empty")
	assert.Equal(0, countSources(t, second), "second fixture starts empty")

	_, err := first.GetOrCreateSource("gmail", "isolation@example.com")
	require.NoError(err, "write through the first fixture")

	assert.Equal(1, countSources(t, first), "write lands in the first fixture")
	assert.Equal(0, countSources(t, second), "write must not leak into the second fixture")
}

// TestPostgresFixtureSchemaDroppedAfterCleanup proves the claiming test still
// owns removal of whatever schema it was handed.
func TestPostgresFixtureSchemaDroppedAfterCleanup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := requirePostgresTestURL(t)

	adminDB, err := pgAdminDB(dbURL)
	require.NoError(err, "open admin connection")

	var schema string
	t.Run("owner", func(t *testing.T) {
		schema = currentSchemaOf(t, NewTestStore(t))
	})

	require.NotEmpty(schema, "subtest recorded its schema")
	assert.False(schemaExists(t, adminDB, schema), "claimed schema is gone after the owning test's cleanup")
}

// TestSweepWarmSchemasDropsOnlyDeadOwners covers the sweeper's liveness rule:
// a warm schema whose creating process is gone is reclaimed, one whose owner is
// still running is left alone.
func TestSweepWarmSchemasDropsOnlyDeadOwners(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := requirePostgresTestURL(t)

	adminDB, err := pgAdminDB(dbURL)
	require.NoError(err, "open admin connection")

	// This is the wired-up case: the real owner token and the real /proc
	// liveness check. Where the host cannot identify its namespace there is no
	// token, the pool is off, and the sweep must be a no-op — assert that
	// rather than pass over the platform, because a sweep that still ran there
	// would be judging pids it cannot resolve.
	if !warmOwnerUsable {
		dropped, err := sweepWarmSchemas(adminDB, warmOwnerToken, processAlive)
		require.NoError(err, "sweep warm schemas")
		assert.Empty(dropped, "a process with no owner token must sweep nothing")

		return
	}

	dead := warmSchemaName(warmOwnerToken, reapedPID(t), randomSchemaSuffix(t))
	self := warmSchemaName(warmOwnerToken, os.Getpid(), randomSchemaSuffix(t))
	otherLive := warmSchemaName(warmOwnerToken, 1, randomSchemaSuffix(t))
	createSchemaForTest(t, adminDB, dead)
	createSchemaForTest(t, adminDB, self)
	createSchemaForTest(t, adminDB, otherLive)

	dropped, err := sweepWarmSchemas(adminDB, warmOwnerToken, processAlive)
	require.NoError(err, "sweep warm schemas")

	assert.Contains(dropped, dead, "sweep reports the reclaimed schema")
	assert.False(schemaExists(t, adminDB, dead), "dead owner's warm schema is reclaimed")
	assert.True(schemaExists(t, adminDB, self), "this process's own warm schema survives")
	assert.True(schemaExists(t, adminDB, otherLive), "a live owner's warm schema survives")
}

// TestSweepWarmSchemasNeverDropsTestSchemas is the safety test: other agents'
// in-flight fixtures use the msgvault_test_ prefix on this shared server, and no
// liveness verdict may ever put one of those in the sweeper's sights.
func TestSweepWarmSchemasNeverDropsTestSchemas(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := requirePostgresTestURL(t)

	adminDB, err := pgAdminDB(dbURL)
	require.NoError(err, "open admin connection")

	// A fixture-shaped schema, plus one that mimics the warm naming inside the
	// test prefix — neither may be touched.
	owner := syntheticOwner(t)
	testSchema := "msgvault_test_" + randomSchemaSuffix(t)
	lookalike := fmt.Sprintf("msgvault_test_%s_p%d_%s", owner, 1, randomSchemaSuffix(t))
	createSchemaForTest(t, adminDB, testSchema)
	createSchemaForTest(t, adminDB, lookalike)

	// Bait: a genuine warm schema owned by a pid this sweep will call dead, so
	// the survival assertions below cannot pass vacuously.
	const strangerPID = 424242
	bait := warmSchemaName(owner, strangerPID, randomSchemaSuffix(t))
	createSchemaForTest(t, adminDB, bait)

	// Declare only the bait's owner dead: sibling test binaries sharing this
	// server keep their warm schemas.
	dropped, err := sweepWarmSchemas(adminDB, owner, func(pid int) bool { return pid != strangerPID })
	require.NoError(err, "sweep warm schemas")

	assert.Contains(dropped, bait, "sweep did drop something in this run")
	for _, name := range dropped {
		assert.Truef(strings.HasPrefix(name, warmSchemaPrefix),
			"sweep may only ever drop names carrying the warm prefix, got %q", name)
	}
	assert.True(schemaExists(t, adminDB, testSchema), "a msgvault_test_ schema must survive the sweep")
	assert.True(schemaExists(t, adminDB, lookalike), "a warm-looking msgvault_test_ schema must survive the sweep")
}

// TestParseWarmSchemaName pins which names the sweeper is even able to consider.
func TestParseWarmSchemaName(t *testing.T) {
	suffix := "0123456789abcdef"

	// A synthetic token, so what this test means does not depend on whether the
	// host running it can identify its own pid namespace.
	owner := strings.Repeat("ab", warmOwnerTokenBytes)

	t.Run("accepts every name warmSchemaName assembles", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)

		name := warmSchemaName(owner, 4242, suffix)

		parsedOwner, pid, parsedSuffix, ok := parseWarmSchemaName(name)
		require.True(ok, "an assembled name must round-trip")
		assert.Equal(owner, parsedOwner, "name carries the owner token")
		assert.Equal(4242, pid, "name carries the creating pid")
		assert.Equal(suffix, parsedSuffix, "name carries the suffix")
	})

	t.Run("accepts a name this package generated", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)

		name, err := newWarmSchemaName()
		require.NoError(err, "generate warm schema name")

		parsedOwner, pid, parsedSuffix, ok := parseWarmSchemaName(name)

		// Where /proc cannot identify the pid namespace there is no owner
		// token, the pool does not run, and nothing it could name is
		// sweepable. Assert that rather than passing over the platform: an
		// ownerless name reaching the sweeper would be the bug.
		if !warmOwnerUsable {
			assert.False(ok, "a name built without an owner token must not be sweepable")

			return
		}

		require.True(ok, "generated names must round-trip")
		assert.Equal(os.Getpid(), pid, "name carries the creating pid")
		assert.Equal(warmOwnerToken, parsedOwner, "name carries this namespace's owner token")
		assert.Equal(name, warmSchemaName(parsedOwner, pid, parsedSuffix), "parts rebuild the name")
	})

	rejected := []string{
		"msgvault_test_" + suffix,
		"msgvault_test_" + owner + "_p1_" + suffix,
		"msgvault_warm_" + suffix,
		"msgvault_warm_" + owner + "_pfoo_" + suffix,
		"msgvault_warm_" + owner + "_p_" + suffix,
		"msgvault_warm_" + owner + "_p-1_" + suffix,
		"msgvault_warm_" + owner + "_p1_" + suffix + "; DROP SCHEMA msgvault_test_x",
		"msgvault_warm_" + owner + "_p1_" + strings.ToUpper(suffix),
		// Owner token of the wrong width or alphabet is not a name we make.
		"msgvault_warm_" + owner + "ab_p1_" + suffix,
		"msgvault_warm_zzzzzzzzzzzz_p1_" + suffix,
		"msgvault_warm_p1_" + suffix,
		"public",
		"",
		" msgvault_warm_" + owner + "_p1_" + suffix,
	}
	for _, name := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			_, _, _, ok := parseWarmSchemaName(name)
			assert.False(t, ok, "name %q must not be sweepable", name)
		})
	}
}

// TestWarmPoolServesInitializedSchemas proves the pool's product is a real,
// already-migrated schema, not just a name.
func TestWarmPoolServesInitializedSchemas(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := requirePostgresTestURL(t)

	adminDB, err := pgAdminDB(dbURL)
	require.NoError(err, "open admin connection")

	// Through claimWarmSchema, not the pool directly: the gate that keeps a
	// host with no owner token from creating schemas nothing could reclaim
	// lives there, and a test that reaches past it would create exactly those.
	name, ok := claimWarmSchema(dbURL)
	if !warmOwnerUsable {
		assert.False(ok, "a host that cannot reclaim warm schemas must not create them")

		return
	}
	require.True(ok, "warm pool produced a schema")
	t.Cleanup(func() { dropOwnedSchema(adminDB, name) })

	assert.Truef(strings.HasPrefix(name, warmSchemaPrefix), "warm schemas are self-owned, got %q", name)

	var messagesTable sql.NullString
	require.NoError(adminDB.QueryRow("SELECT to_regclass($1)", name+".messages").Scan(&messagesTable),
		"look up the messages table in the warm schema")
	assert.True(messagesTable.Valid, "warm schema already carries the initialized DDL")
}

// TestNewTestStoreFallsBackWhenWarmPoolDisabled covers the path a pool outage
// takes: the fixture creates its own schema and behaves exactly as before.
func TestNewTestStoreFallsBackWhenWarmPoolDisabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requirePostgresTestURL(t)
	t.Setenv(warmPoolDisableEnv, "0")

	st := NewTestStore(t)

	schema := currentSchemaOf(t, st)
	assert.Truef(strings.HasPrefix(schema, "msgvault_test_"),
		"the fallback path creates its own schema, got %q", schema)
	assert.Equal(0, countSources(t, st), "fallback fixture starts empty")

	_, err := st.GetOrCreateSource("gmail", "fallback@example.com")
	require.NoError(err, "write through the fallback fixture")
	assert.Equal(1, countSources(t, st), "fallback fixture is writable")
}

// TestSweepWarmSchemasNeverDropsForeignNamespaces is the cross-namespace safety
// test. A pid only means something relative to a namespace, so when two
// containers share one PostgreSQL server each sees a /proc the other's pids are
// absent from. Without an owner token the sweeper reads that absence as "the
// owner exited" and drops a schema whose test is still running — a warm schema
// keeps its name for the whole life of the test that claims it, so the victim
// is live, not spare.
//
// The foreign schema here is built to be maximally tempting: its pid is one
// this process's /proc genuinely cannot find. Only the owner token stands
// between it and deletion.
func TestSweepWarmSchemasNeverDropsForeignNamespaces(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := requirePostgresTestURL(t)

	adminDB, err := pgAdminDB(dbURL)
	require.NoError(err, "open admin connection")

	// Same shape, same dead pid; only the namespace differs. Both owners are
	// synthetic and liveness is injected, so the question this poses does not
	// depend on the host having a /proc to be absent from.
	owner := syntheticOwner(t)
	foreignOwner := syntheticOwner(t)
	require.NotEqual(owner, foreignOwner, "the foreign token must not be ours")

	const deadPID = 424243
	foreign := warmSchemaName(foreignOwner, deadPID, randomSchemaSuffix(t))
	ours := warmSchemaName(owner, deadPID, randomSchemaSuffix(t))
	createSchemaForTest(t, adminDB, foreign)
	createSchemaForTest(t, adminDB, ours)

	dropped, err := sweepWarmSchemas(adminDB, owner, func(pid int) bool { return false })
	require.NoError(err, "sweep warm schemas")

	// Our own orphan proves the sweep ran and would have taken the foreign one
	// had the token not stopped it.
	assert.Contains(dropped, ours, "our own namespace's orphan is still reclaimed")
	assert.NotContains(dropped, foreign, "a foreign namespace's schema must never be swept")
	assert.True(schemaExists(t, adminDB, foreign),
		"another container's warm schema survives: its pid is not ours to judge")
}

// drainWarmPool empties a pool's buffer and drops what it held, so the next
// fixture is the one that pays for a refill.
func drainWarmPool(t *testing.T, pool *warmSchemaPool) {
	t.Helper()

	adminDB, err := pgAdminDB(pool.dbURL)
	require.NoError(t, err, "open admin connection")

	pool.mu.Lock()
	defer pool.mu.Unlock()

	for _, name := range pool.names {
		dropOwnedSchema(adminDB, name)
	}
	pool.names = nil
}

// TestWarmPoolIssuesNoStatementsOnceATestBodyIsRunning is the invariant that
// decides the pool's shape. Several tests in this repository capture
// slog.Default() and assert over the whole captured buffer, and the migration
// paths carry seams; all of that is safe only while exactly one migration is on
// the wire at a time. A pool that warmed in the background broke it — an
// upstream test asserting that a query never touches message_raw failed on a
// pool worker's migration writing that table underneath it.
//
// Building inside the claiming fixture's own setup is what closes that off: the
// pool's statements land where fixture statements already landed, so a test body
// observes nothing it would not have observed without the pool. The buffer is
// drained first so the fixture below genuinely triggers a refill, which is
// exactly the moment the background design would have kept working.
func TestWarmPoolIssuesNoStatementsOnceATestBodyIsRunning(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := requirePostgresTestURL(t)

	drainWarmPool(t, warmPoolFor(dbURL))
	fixture := NewTestStore(t)

	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true, MaxStmtChars: 10_000})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	var logged bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	// Real work, and enough of it to give any stray warming the wall clock it
	// would need to appear in the capture.
	for i := range 20 {
		_, err := fixture.GetOrCreateSource("gmail", fmt.Sprintf("warm-quiet-%d@example.test", i))
		require.NoError(err, "write through the fixture")
	}

	trace := strings.ToLower(logged.String())
	require.Contains(trace, "sources", "the capture observed this test's own queries")
	assert.NotContains(trace, "create table",
		"pool migrations must not run while a test body is capturing the process logger")
	assert.NotContains(trace, "create index",
		"pool migrations must not run while a test body is capturing the process logger")
	assert.NotContains(trace, warmSchemaPrefix,
		"no pool schema may be named in a statement issued during a test body")
}

// TestWarmOwnerRequiresBothNamespaceIdentifiers pins the rule that keeps the
// pool from leaking on hosts it cannot clean up after, and from mistaking a
// stranger for itself.
//
// The sweep reclaims an orphan by asking /proc whether its creator exited, so
// where the namespace cannot be identified there is no token and the pool
// declines to run. Neither identifier suffices alone: a boot id is common to
// every container on a machine, and a namespace inode is common to every
// machine that numbers its namespaces alike. Either on its own would let two
// processes that cannot see each other's /proc agree on a token — the exact
// condition under which one deletes the other's live schema.
func TestWarmOwnerRequiresBothNamespaceIdentifiers(t *testing.T) {
	assert := assert.New(t)

	bootID := []byte("boot-id")
	namespace := []byte("pid:[4026531836]")

	_, ok := warmOwnerFrom(nil, nil)
	assert.False(ok, "a host that identifies neither must not run the pool")

	_, ok = warmOwnerFrom(bootID, nil)
	assert.False(ok, "a boot id alone is shared by every container on the host")

	_, ok = warmOwnerFrom(nil, namespace)
	assert.False(ok, "a namespace id alone is shared by every host that numbers alike")

	token, ok := warmOwnerFrom(bootID, namespace)
	assert.True(ok, "both identifiers yield a token")
	assert.Len(token, warmOwnerTokenBytes*2, "the token is the width the name pattern expects")

	sameHost, _ := warmOwnerFrom(bootID, []byte("pid:[4026532999]"))
	assert.NotEqual(token, sameHost, "two namespaces on one host must not share a token")

	sameNamespace, _ := warmOwnerFrom([]byte("another-boot-id"), namespace)
	assert.NotEqual(token, sameNamespace, "two hosts numbering alike must not share a token")
}

// TestWarmPoolBatchWidthOnlyEverNarrows covers the connection-budget knob. The
// constant is a ceiling on how many connections one refill holds, so an
// environment able to raise it would not be a safety limit at all.
func TestWarmPoolBatchWidthOnlyEverNarrows(t *testing.T) {
	ignored := []string{"", "0", "-1", "abc", "1.5", strconv.Itoa(warmPoolBatch + 1), "999"}
	for _, value := range ignored {
		t.Run("ignores "+value, func(t *testing.T) {
			t.Setenv(warmPoolBatchEnv, value)
			assert.Equal(t, warmPoolBatch, warmPoolBatchWidth(),
				"%q must not change the refill width", value)
		})
	}

	t.Run("accepts a narrower width", func(t *testing.T) {
		t.Setenv(warmPoolBatchEnv, "1")
		assert.Equal(t, 1, warmPoolBatchWidth(), "the environment may lower the connection footprint")
	})
}
