package testutil

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx driver for test setup
	"go.kenn.io/msgvault/internal/store"
)

// The PostgreSQL fixture's cost is almost entirely CREATE SCHEMA plus the
// InitSchema() DDL replay, and that work is round-trip bound rather than CPU
// bound. This file makes fixtures pay for it in batches instead of one at a
// time: a fixture that finds the buffer empty builds warmPoolBatch schemas
// concurrently — barely more wall clock than the one it needed — and the
// fixtures that follow claim theirs for free. Every fixture still gets a
// private, never-used schema built by the same InitSchema() code path, and
// still drops it in t.Cleanup — nothing is shared, reused, or truncated.
const (
	// warmSchemaPrefix marks a schema as pool-owned. A full name also carries
	// the owner token (see warmOwnerToken) and the pid that created it. The
	// prefix deliberately does not overlap the msgvault_test_ prefix used for
	// directly created fixtures: the orphan sweep only ever acts on names
	// carrying this prefix, so a fixture schema — including one belonging to an
	// unrelated process sharing the server — can never be a candidate.
	warmSchemaPrefix = "msgvault_warm_"

	// warmOwnerTokenBytes is the width of the owner token in the schema name.
	warmOwnerTokenBytes = 6

	// warmPoolBatch is how many schemas one refill builds, and therefore how
	// many connections that refill holds while it runs. It is a host-safety
	// limit, not a tuning knob, and must never be derived from GOMAXPROCS or
	// the -p flag. A shared test server offers ~97 usable connections; a refill
	// holds one per schema it is building, and the shared admin handle adds two
	// more, so a test binary peaks at roughly six.
	//
	// That bound is per test binary. `go test ./...` builds one binary per
	// package and runs up to -p of them at once, so a wide fan-out against a
	// small shared server can still exceed its limit in aggregate; there is no
	// cross-process budget here. The Makefile's PostgreSQL targets bound the
	// other side with -p (PG_TEST_PARALLEL), and warmPoolBatchEnv lowers this
	// one. The failure mode is not corruption: a refill that cannot connect
	// gives up and fixtures create their own schemas exactly as they did before
	// the pool existed.
	warmPoolBatch = 4

	// warmPoolDisableEnv set to "0" turns the pool off and sends every fixture
	// down the direct-creation path. An escape hatch for diagnosing whether the
	// pool is implicated in a failure.
	warmPoolDisableEnv = "MSGVAULT_TEST_PG_WARM_POOL"

	// warmPoolBatchEnv narrows the refill for a server with a tighter
	// connection budget than the arithmetic above assumes. It can only lower
	// warmPoolBatch: a knob able to raise a safety limit is not a safety limit.
	warmPoolBatchEnv = "MSGVAULT_TEST_PG_WARM_POOL_BATCH"

	// warmPoolMaxFailures stops refilling after this many consecutive empty
	// refills, so a server outage does not make every remaining fixture pay for
	// a failed batch first. Fixtures fall back to direct creation and fail with
	// the error they would have reported without the pool.
	warmPoolMaxFailures = 3

	// warmSchemaSuffixBytes is the entropy in a schema's random suffix.
	warmSchemaSuffixBytes = 8
)

// warmSchemaNamePattern is the sole gate on what the sweep may consider. It is
// built from warmSchemaPrefix so the prefix has one definition.
var warmSchemaNamePattern = regexp.MustCompile(
	"^" + regexp.QuoteMeta(warmSchemaPrefix) +
		"([0-9a-f]{" + strconv.Itoa(warmOwnerTokenBytes*2) + "})" +
		"_p([0-9]{1,10})_([0-9a-f]{" + strconv.Itoa(warmSchemaSuffixBytes*2) + "})$")

// warmOwnerToken names the PID namespace that this process's /proc describes,
// and warmOwnerUsable reports whether the pool may run at all.
//
// A pid is only meaningful relative to a namespace. Two containers sharing one
// PostgreSQL server each see their own /proc, so a sweeper in one of them can
// look up a pid belonging to the other, find nothing, and conclude the owner
// has exited — while that owner is mid-test. A warm schema keeps its name for
// the whole life of the test that claims it, so the schema being deleted is a
// live one, and the test loses its tables underneath it.
//
// Encoding the namespace in the name closes that off: the sweep skips every
// candidate whose token is not its own, so it only ever judges liveness for
// pids its own /proc can resolve. Foreign orphans are left for the namespace
// that created them to reclaim, which is the trade this sweep should make —
// a leaked schema costs a name until that binary runs again, and a wrong
// verdict corrupts a running test.
var warmOwnerToken, warmOwnerUsable = deriveWarmOwner()

// deriveWarmOwner builds the owner token from the boot id and the pid namespace
// — the two things that decide whether another process's pid is resolvable
// through our /proc.
//
// It reports false unless both are readable, and that is the point rather than
// a limitation. The sweep reclaims an orphan by asking /proc whether the process
// that created it has exited, so a host that cannot answer that question can
// never reclaim anything this pool leaves behind: every run would strand its
// buffer permanently. Rather than leak on those hosts, the pool stays off there
// and fixtures create their own schemas exactly as they always did.
func deriveWarmOwner() (string, bool) {
	var bootID []byte
	if read, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		bootID = read
	}
	var namespace []byte
	if link, err := os.Readlink("/proc/self/ns/pid"); err == nil {
		namespace = []byte(link)
	}

	return warmOwnerFrom(bootID, namespace)
}

// warmOwnerFrom hashes the two identifiers into the token that goes in a schema
// name, reporting false when either is missing.
//
// Both are required, and neither is redundant. A boot id is shared by every
// container on the machine, so on its own it would give two containers the same
// token and put us straight back to sweeping each other's live schemas. A
// namespace inode number is shared by every machine that happens to number its
// namespaces alike — the initial namespace has the same well-known id
// everywhere — so on its own it would do the same across hosts. Only the pair
// identifies "a /proc that can resolve these pids".
//
// Split out from deriveWarmOwner so the rule is testable without a host that
// lacks either.
func warmOwnerFrom(bootID, namespace []byte) (string, bool) {
	if len(bootID) == 0 || len(namespace) == 0 {
		return "", false
	}

	digest := sha256.New()
	_, _ = digest.Write(bootID)
	_, _ = digest.Write(namespace)

	return hex.EncodeToString(digest.Sum(nil)[:warmOwnerTokenBytes]), true
}

var (
	adminDBMu  sync.Mutex
	adminDBs   = map[string]*sql.DB{}
	warmPoolMu sync.Mutex
	warmPools  = map[string]*warmSchemaPool{}
)

// pgAdminDB returns the process-wide administrative handle for a database URL,
// opening it on first use. Fixtures previously paid a TCP and authentication
// handshake to create a schema and another to drop it; sql.DB is already a
// pool, so one handle per URL serves every fixture in the binary. It is capped
// small and intentionally never closed: it lives as long as the test binary.
func pgAdminDB(dbURL string) (*sql.DB, error) {
	adminDBMu.Lock()
	defer adminDBMu.Unlock()

	if db, ok := adminDBs[dbURL]; ok {
		return db, nil
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	adminDBs[dbURL] = db

	return db, nil
}

// warmSchemaPool hands out names of schemas that have already been created and
// migrated. It serves names rather than open stores, so the buffer holds no
// database connections at rest; the claiming test opens its own handle.
//
// The pool owns no background goroutine, and that is a correctness property
// rather than a simplification. A refill runs synchronously inside the fixture
// that emptied the buffer, so every statement the pool issues lands in the same
// window where that fixture's own CREATE SCHEMA and InitSchema already landed —
// never while a test body is running. Tests in this repository observe
// process-global state: several capture slog.Default() and assert over the whole
// buffer, and the migration paths carry seams. Warming in the background put a
// second migration on the wire underneath those tests and broke them. Warming
// inside fixture setup cannot, because it adds no overlap that fixture creation
// did not already have.
type warmSchemaPool struct {
	dbURL string

	mu       sync.Mutex
	names    []string
	failures int
	swept    bool
}

// warmPoolFor returns the pool for a database URL, creating it on first use.
// Nothing happens until a PostgreSQL fixture is requested, so SQLite runs and
// packages that never touch PostgreSQL pay nothing.
func warmPoolFor(dbURL string) *warmSchemaPool {
	warmPoolMu.Lock()
	defer warmPoolMu.Unlock()

	if pool, ok := warmPools[dbURL]; ok {
		return pool
	}

	pool := &warmSchemaPool{dbURL: dbURL}
	warmPools[dbURL] = pool

	return pool
}

// claim takes a ready schema name, refilling the buffer first when it is empty.
// It reports false rather than an error: a fixture the pool cannot serve creates
// its own schema and surfaces any real failure itself, so the pool can only make
// a run faster, never fail one.
func (p *warmSchemaPool) claim() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.names) == 0 {
		p.refillLocked()
	}
	if len(p.names) == 0 {
		return "", false
	}

	name := p.names[len(p.names)-1]
	p.names = p.names[:len(p.names)-1]

	return name, true
}

// refillLocked builds a batch of schemas concurrently. Concurrency is where the
// saving comes from: the work is round-trip bound rather than CPU bound, so a
// batch costs little more than the one schema this fixture needed anyway and the
// rest are free to the fixtures that follow. It stops refilling for good after
// warmPoolMaxFailures empty batches so an unreachable server does not make every
// remaining fixture pay for a doomed round trip first.
func (p *warmSchemaPool) refillLocked() {
	if p.failures >= warmPoolMaxFailures {
		return
	}
	if !p.swept {
		p.swept = true
		if db, err := pgAdminDB(p.dbURL); err == nil {
			_, _ = sweepWarmSchemas(db, warmOwnerToken, processAlive)
		}
	}

	built := make([]string, warmPoolBatchWidth())
	var building sync.WaitGroup
	for i := range built {
		building.Go(func() {
			if name, err := p.warmOne(); err == nil {
				built[i] = name
			}
		})
	}
	building.Wait()

	for _, name := range built {
		if name != "" {
			p.names = append(p.names, name)
		}
	}

	if len(p.names) == 0 {
		p.failures++

		return
	}
	p.failures = 0
}

// warmPoolBatchWidth is the refill width this binary uses: warmPoolBatch, or a
// smaller value from warmPoolBatchEnv. Anything unparseable, below one, or above
// the constant is ignored — the constant is a ceiling on the connection
// footprint, so the environment may only lower it.
func warmPoolBatchWidth() int {
	width, err := strconv.Atoi(os.Getenv(warmPoolBatchEnv))
	if err != nil || width < 1 || width > warmPoolBatch {
		return warmPoolBatch
	}

	return width
}

// warmOne creates a schema, replays the production DDL into it, and closes the
// connection it used, leaving a ready-to-claim schema and no open connection.
func (p *warmSchemaPool) warmOne() (string, error) {
	db, err := pgAdminDB(p.dbURL)
	if err != nil {
		return "", err
	}

	name, err := newWarmSchemaName()
	if err != nil {
		return "", err
	}

	if _, err := db.Exec("CREATE SCHEMA " + name); err != nil {
		return "", err
	}

	st, err := store.Open(schemaURL(p.dbURL, name))
	if err != nil {
		dropOwnedSchema(db, name)

		return "", err
	}

	if err := st.InitSchema(); err != nil {
		_ = st.Close()
		dropOwnedSchema(db, name)

		return "", err
	}

	if err := st.Close(); err != nil {
		dropOwnedSchema(db, name)

		return "", err
	}

	return name, nil
}

// claimWarmSchema takes a ready schema, building a batch first when the buffer
// is empty. It is off entirely when the escape hatch is set, and on a host whose
// /proc cannot identify the pid namespace, because nothing the pool left behind
// there could ever be reclaimed.
func claimWarmSchema(dbURL string) (string, bool) {
	if os.Getenv(warmPoolDisableEnv) == "0" || !warmOwnerUsable {
		return "", false
	}

	return warmPoolFor(dbURL).claim()
}

// newWarmSchemaName returns a fresh warm schema name owned by this process.
func newWarmSchemaName() (string, error) {
	buf := make([]byte, warmSchemaSuffixBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random schema name: %w", err)
	}

	return warmSchemaName(warmOwnerToken, os.Getpid(), hex.EncodeToString(buf)), nil
}

// warmSchemaName assembles a name from validated parts. Every name this package
// creates, and every name its sweep drops, is built here.
func warmSchemaName(owner string, pid int, suffix string) string {
	return fmt.Sprintf("%s%s_p%d_%s", warmSchemaPrefix, owner, pid, suffix)
}

// parseWarmSchemaName splits a warm schema name into the owner token and pid
// that created it, plus its random suffix. Anything that is not exactly a name
// this package generates — including every msgvault_test_ fixture schema — is
// rejected.
func parseWarmSchemaName(name string) (owner string, pid int, suffix string, ok bool) {
	match := warmSchemaNamePattern.FindStringSubmatch(name)
	if match == nil {
		return "", 0, "", false
	}

	pid, err := strconv.Atoi(match[2])
	if err != nil || pid <= 0 {
		return "", 0, "", false
	}

	return match[1], pid, match[3], true
}

// sweepWarmSchemas reclaims the warm schemas owned by owner whose creating
// process has exited — the buffer a test binary leaves behind when it stops. It
// returns the names it dropped, and does nothing at all for an empty owner,
// which is how a host that cannot identify its own pid namespace sweeps.
//
// Ownership and liveness are parameters rather than globals so a test can pose
// the question this function actually answers on any platform, without a
// synthetic pid having to be absent from a /proc the host may not have.
//
// Three properties matter more than completeness. First, the statement it
// executes is rebuilt from the parsed parts, so its target always begins with
// warmSchemaPrefix; a msgvault_test_ schema cannot be named by this function no
// matter what the server returns. Second, a candidate owned by another pid
// namespace is skipped outright — its pid is not ours to resolve, so no
// liveness answer about it could be trusted. Third, liveness fails safe toward
// "alive": an unreadable /proc, an unsupported platform, or a name it cannot
// parse leaves the schema alone. A missed orphan costs a schema until the
// owning binary runs again; a wrong verdict would delete a running test's data.
func sweepWarmSchemas(db *sql.DB, owner string, alive func(pid int) bool) ([]string, error) {
	if owner == "" {
		return nil, nil
	}

	candidates, err := listWarmSchemas(db)
	if err != nil {
		return nil, err
	}

	var dropped []string
	self := os.Getpid()
	for _, candidate := range candidates {
		candidateOwner, pid, suffix, ok := parseWarmSchemaName(candidate)
		if !ok || candidateOwner != owner || pid == self || alive(pid) {
			continue
		}

		// Rebuilt from validated parts, never interpolated from the row.
		target := warmSchemaName(candidateOwner, pid, suffix)
		if target != candidate {
			continue
		}

		if _, err := db.Exec("DROP SCHEMA " + target + " CASCADE"); err != nil {
			continue
		}
		dropped = append(dropped, target)
	}

	return dropped, nil
}

// listWarmSchemas returns the schema names carrying the warm prefix. The LIKE
// pattern is derived from the prefix constant with its underscores escaped, so
// the server is never asked about any other family of schema.
func listWarmSchemas(db *sql.DB) ([]string, error) {
	pattern := strings.ReplaceAll(warmSchemaPrefix, "_", `\_`) + "%"
	rows, err := db.Query("SELECT nspname FROM pg_namespace WHERE nspname LIKE $1", pattern)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return names, nil
}

// processAlive reports whether a pid is still running, answering "yes" whenever
// it cannot tell. Only Linux is decidable here, via /proc.
func processAlive(pid int) bool {
	if pid <= 0 || runtime.GOOS != "linux" {
		return true
	}

	if _, err := os.Stat("/proc/self"); err != nil {
		return true
	}

	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err == nil {
		return true
	}

	return !errors.Is(err, fs.ErrNotExist)
}

// fixtureSchemaNamePattern matches the schemas createTestSchema builds, so a
// drop can rebuild its target from validated parts the same way the sweep does.
var fixtureSchemaNamePattern = regexp.MustCompile(
	"^msgvault_test_([0-9a-f]{" + strconv.Itoa(warmSchemaSuffixBytes*2) + "})$")

// dropOwnedSchema removes a schema this package created, and does nothing at
// all for any other name. Failure is ignored: the sweep reclaims what is left
// behind.
//
// The statement is assembled from the parts a pattern matched rather than from
// the caller's string — the same discipline sweepWarmSchemas applies. A name
// that did not come from this package cannot become SQL here, whatever a future
// caller passes in.
func dropOwnedSchema(db *sql.DB, name string) {
	var target string
	switch fixture := fixtureSchemaNamePattern.FindStringSubmatch(name); {
	case fixture != nil:
		target = "msgvault_test_" + fixture[1]
	default:
		owner, pid, suffix, ok := parseWarmSchemaName(name)
		if !ok {
			return
		}
		target = warmSchemaName(owner, pid, suffix)
	}

	//nolint:gosec // target is rebuilt from matched parts above, not the argument.
	_, _ = db.Exec("DROP SCHEMA IF EXISTS " + target + " CASCADE")
}

// schemaURL returns dbURL with its search_path pointed at a schema.
func schemaURL(dbURL, schemaName string) string {
	separator := "?"
	if strings.Contains(dbURL, "?") {
		separator = "&"
	}

	return dbURL + separator + "search_path=" + schemaName
}
