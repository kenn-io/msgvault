package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"sync"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx driver for test setup
	"go.kenn.io/msgvault/internal/store"
)

// A PostgreSQL fixture used to create a schema and replay InitSchema() into it:
// a few hundred round trips, ~1.5s against a local server and much more on a
// contended CI runner, paid by every fixture. The initialized schema is a pure
// function of the test binary, so each binary builds it once into a template
// database and every fixture is then a CREATE DATABASE ... TEMPLATE clone —
// a file-level copy the server makes in tens of milliseconds. Each fixture
// still gets a private, never-used database produced by the same InitSchema()
// path, and drops it on cleanup.
//
// Ownership of a template is a session-level advisory lock held on a
// connection pinned for the life of the binary. The server releases it the
// moment that session ends — a clean exit, a panic, or a SIGKILL all look the
// same — so the next binary to start can tell a live template from an
// abandoned one without consulting the host: whatever it can lock, nobody
// owns. That is the whole reclamation story; no pid, boot id, or /proc is
// involved, so it holds on every platform and across containers sharing one
// server.
const (
	// templateDBPrefix marks a template database as owned by this fixture. The
	// full name carries the owner token whose advisory lock the building binary
	// holds. It deliberately does not overlap the msgvault_test_ prefix of
	// per-schema fixtures, or the configured database's own name.
	templateDBPrefix = "msgvault_tt_"

	// cloneDBPrefix marks a per-test clone. The name carries the template's
	// owner token, so a sweep that reclaims a template reclaims its clones too.
	cloneDBPrefix = "msgvault_tc_"

	// templateTokenBytes is the width of the owner token: eight bytes, so it is
	// also the advisory lock key.
	templateTokenBytes = 8

	// templateDisableEnv set to "0" turns template cloning off and sends every
	// fixture down the per-schema path. An escape hatch for diagnosing whether
	// cloning is implicated in a failure.
	templateDisableEnv = "MSGVAULT_TEST_PG_TEMPLATE"
)

// templateNamePattern and cloneNamePattern are the sole gates on what the
// sweep may consider and what dropOwnedDatabase may drop.
var (
	templateNamePattern = regexp.MustCompile("^" + templateDBPrefix + "([0-9a-f]{16})$")
	cloneNamePattern    = regexp.MustCompile("^" + cloneDBPrefix + "([0-9a-f]{16})_([0-9a-f]{16})$")
)

var (
	adminDBMu   sync.Mutex
	adminDBs    = map[string]*sql.DB{}
	templatesMu sync.Mutex
	templates   = map[string]*pgTemplate{}
)

// pgAdminDB returns the process-wide administrative handle for a database URL,
// opening it on first use. One handle per URL serves every fixture in the
// binary. It is capped small and intentionally never closed: it lives as long
// as the test binary, and the template's ownership lock is pinned on one of
// its connections.
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
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(3)
	adminDBs[dbURL] = db

	return db, nil
}

// pgTemplate is the per-binary template for one database URL.
type pgTemplate struct {
	dbURL string

	mu    sync.Mutex
	token string    // set once the template is built
	err   error     // set once building failed; the fixture falls back for good
	owner *sql.Conn // pinned session holding the ownership lock; never closed
}

// templateFor returns the template for a database URL, creating the record on
// first use. Nothing touches the server until a fixture asks for a clone.
func templateFor(dbURL string) *pgTemplate {
	templatesMu.Lock()
	defer templatesMu.Unlock()

	if tmpl, ok := templates[dbURL]; ok {
		return tmpl
	}

	tmpl := &pgTemplate{dbURL: dbURL}
	templates[dbURL] = tmpl

	return tmpl
}

// name returns the template database's name.
func (p *pgTemplate) name() string {
	return templateDBPrefix + p.token
}

// ensure builds the template on first use and reports whether it is usable. A
// build failure — typically a role without CREATEDB — is remembered so later
// fixtures fall straight back to the per-schema path.
func (p *pgTemplate) ensure() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.token != "" || p.err != nil {
		return p.err
	}

	p.token, p.owner, p.err = buildTemplate(p.dbURL)

	return p.err
}

// buildTemplate creates and initializes a template database, returning its
// owner token and the pinned session that holds the ownership lock. The lock
// is taken before the database exists so no sweep can ever see an unlocked
// template of ours; the last-used-connection ordering at the end is what lets
// the server accept the template as a clone source.
func buildTemplate(dbURL string) (string, *sql.Conn, error) {
	admin, err := pgAdminDB(dbURL)
	if err != nil {
		return "", nil, err
	}
	ctx := context.Background()

	// Best effort: reclaim what earlier binaries left behind.
	_, _ = sweepOrphanTemplates(ctx, admin)

	token, err := newTemplateToken()
	if err != nil {
		return "", nil, err
	}

	owner, err := admin.Conn(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("pin template owner session: %w", err)
	}
	if _, err := owner.ExecContext(ctx, "SELECT pg_advisory_lock($1)", templateLockKey(token)); err != nil {
		_ = owner.Close()

		return "", nil, fmt.Errorf("take template ownership lock: %w", err)
	}
	release := func() {
		_, _ = owner.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", templateLockKey(token))
		_ = owner.Close()
	}

	name := templateDBPrefix + token
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		release()

		return "", nil, fmt.Errorf("create template database: %w", err)
	}
	if err := initTemplate(ctx, admin, dbURL, name); err != nil {
		dropOwnedDatabase(admin, name)
		release()

		return "", nil, err
	}

	return token, owner, nil
}

// initTemplate makes a fresh template database match the configured one: the
// same extensions, then the production schema through InitSchema().
func initTemplate(ctx context.Context, admin *sql.DB, dbURL, name string) error {
	extensions, err := installedExtensions(ctx, admin)
	if err != nil {
		return err
	}

	st, err := store.Open(withDatabase(dbURL, name))
	if err != nil {
		return fmt.Errorf("open template database: %w", err)
	}
	for _, extension := range extensions {
		statement := "CREATE EXTENSION IF NOT EXISTS " + pgx.Identifier{extension}.Sanitize()
		//nolint:gosec // extension names come from pg_extension and are quoted by pgx.Identifier.
		if _, err := st.DB().ExecContext(ctx, statement); err != nil {
			_ = st.Close()

			return fmt.Errorf("install extension %s in template: %w", extension, err)
		}
	}
	if err := st.InitSchema(); err != nil {
		_ = st.Close()

		return fmt.Errorf("init template schema: %w", err)
	}
	if err := st.Close(); err != nil {
		return fmt.Errorf("close template database: %w", err)
	}

	return nil
}

// installedExtensions lists the extensions present in the configured database,
// which is where a test server's operator installs them (pgvector, say).
func installedExtensions(ctx context.Context, admin *sql.DB) ([]string, error) {
	rows, err := admin.QueryContext(ctx, "SELECT extname FROM pg_extension WHERE extname <> 'plpgsql' ORDER BY extname")
	if err != nil {
		return nil, fmt.Errorf("list extensions: %w", err)
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

	return names, rows.Err()
}

// clone creates a fresh database from the template and returns its name.
func (p *pgTemplate) clone(ctx context.Context) (string, error) {
	admin, err := pgAdminDB(p.dbURL)
	if err != nil {
		return "", err
	}
	suffix := make([]byte, templateTokenBytes)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("random clone name: %w", err)
	}

	name := cloneDBPrefix + p.token + "_" + hex.EncodeToString(suffix)
	//nolint:gosec // both names are prefixes plus hex tokens generated here.
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name+" TEMPLATE "+p.name()); err != nil {
		return "", fmt.Errorf("clone template database: %w", err)
	}

	return name, nil
}

// newTemplateToken returns a fresh owner token.
func newTemplateToken() (string, error) {
	buf := make([]byte, templateTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random template token: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// templateLockKey is the advisory lock key for an owner token: the token's
// eight bytes read as an integer. Advisory locks are scoped to the database a
// session is connected to, and every party here — builder and sweepers alike
// — takes them through the admin handle on the configured database.
func templateLockKey(token string) int64 {
	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) != templateTokenBytes {
		return 0
	}

	return int64(binary.BigEndian.Uint64(raw)) //nolint:gosec // deliberate reinterpretation of eight random bytes
}

// ownedDatabaseToken returns the owner token carried by a template or clone
// name, or false for any other name. Every name this package creates, and
// every name it drops, passes through here.
func ownedDatabaseToken(name string) (string, bool) {
	if match := templateNamePattern.FindStringSubmatch(name); match != nil {
		return match[1], true
	}
	if match := cloneNamePattern.FindStringSubmatch(name); match != nil {
		return match[1], true
	}

	return "", false
}

// dropOwnedDatabase removes a template or clone this package created, and does
// nothing at all for any other name. The name reaches SQL only after matching
// one of the two patterns, so the configured database or anyone's
// msgvault_test_ schema cannot be named here whatever a caller passes in.
// FORCE terminates a straggling session — a pool that has not finished
// closing — rather than leaking the database over it.
func dropOwnedDatabase(admin *sql.DB, name string) {
	_ = dropOwnedDatabaseOn(context.Background(), admin, name)
}

// execer is the part of *sql.DB and *sql.Conn a drop needs.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// dropOwnedDatabaseOn is dropOwnedDatabase on a caller-chosen connection, for
// the sweep, which must not need a second pooled connection while it holds one.
func dropOwnedDatabaseOn(ctx context.Context, on execer, name string) error {
	if _, ok := ownedDatabaseToken(name); !ok {
		return nil
	}

	_, err := on.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")

	return err
}

// sweepOrphanTemplates reclaims templates, and the clones made from them, whose
// owning binary is gone. Ownership is decided by the server: a token whose
// advisory lock this sweep can take has no live owner, because a live owner
// holds that lock on a pinned session until it exits. A token it cannot lock
// belongs to a running binary and is left alone, clones included. It returns
// the names it dropped.
func sweepOrphanTemplates(ctx context.Context, admin *sql.DB) ([]string, error) {
	names, err := listOwnedDatabases(ctx, admin)
	if err != nil {
		return nil, err
	}
	byToken := map[string][]string{}
	for _, name := range names {
		if token, ok := ownedDatabaseToken(name); ok {
			byToken[token] = append(byToken[token], name)
		}
	}

	var dropped []string
	for token, names := range byToken {
		conn, err := admin.Conn(ctx)
		if err != nil {
			return dropped, err
		}
		var unowned bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", templateLockKey(token)).Scan(&unowned); err != nil {
			_ = conn.Close()

			return dropped, err
		}
		if unowned {
			for _, name := range names {
				if err := dropOwnedDatabaseOn(ctx, conn, name); err == nil {
					dropped = append(dropped, name)
				}
			}
			// Session locks outlive the statement; release before the
			// connection goes back to the pool.
			_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", templateLockKey(token))
		}
		_ = conn.Close()
	}

	return dropped, nil
}

// listOwnedDatabases returns the database names carrying either owned prefix.
func listOwnedDatabases(ctx context.Context, admin *sql.DB) ([]string, error) {
	rows, err := admin.QueryContext(ctx,
		`SELECT datname FROM pg_database WHERE datname LIKE $1 OR datname LIKE $2`,
		likePrefix(templateDBPrefix), likePrefix(cloneDBPrefix))
	if err != nil {
		return nil, fmt.Errorf("list owned databases: %w", err)
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

	return names, rows.Err()
}

// likePrefix escapes a prefix for LIKE so its underscores match literally.
func likePrefix(prefix string) string {
	escaped := make([]byte, 0, len(prefix)+4)
	for i := range len(prefix) {
		if prefix[i] == '_' || prefix[i] == '%' || prefix[i] == '\\' {
			escaped = append(escaped, '\\')
		}
		escaped = append(escaped, prefix[i])
	}

	return string(escaped) + "%"
}

// withDatabase returns dbURL pointed at another database on the same server.
func withDatabase(dbURL, name string) string {
	parsed, err := url.Parse(dbURL)
	if err != nil {
		return dbURL
	}
	parsed.Path = "/" + name

	return parsed.String()
}
