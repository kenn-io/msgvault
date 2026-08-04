package store_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx for the second-role connections
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// These tests are about the ONE thing the shared test store cannot express: a
// connection belonging to a DIFFERENT PostgreSQL role.
//
// PostgreSQL redacts pg_stat_activity — xact_start, state and backend_type all
// read as NULL — for backends belonging to another role, unless the reader is a
// superuser or a member of pg_read_all_stats. That redaction is the only reason
// the commit bound needs a visibility floor at all, and it cannot be reproduced
// by connecting twice as the same role, nor by the throwaway container's owner
// role, which is a superuser and sees everything.
//
// So these tests create two ordinary login roles of their own: one the store
// connects as, one standing in for whatever else reaches the database —
// autovacuum, pg_dump, a metrics exporter, a DBA's psql, a second application.
// Everything they create is dropped again on cleanup.
//
// The property under test is the direction each filter fails in. The redaction
// count gates the floor, and the floor is what stops the feed. A count that
// matches too little fails OPEN: the floor lifts and the bound quietly returns
// to the clock, which is the row loss the bound exists to prevent. A count that
// matches too much fails CLOSED: the feed freezes for everyone until the
// foreign backend goes away. Both are failures; only one is silent, which is
// why the count is scoped to backends that could actually be holding a stamp
// rather than to every backend that exists.

// feedProbeRolePassword is the login password for the two throwaway roles these
// tests create. It guards nothing: both roles exist only inside the disposable
// test container, for the length of one test, and are dropped afterwards.
const feedProbeRolePassword = "feedprobe"

// foreignRoleFeed is a store connected as an ordinary non-superuser role, plus
// a second login role that can open its own connections to the same database.
type foreignRoleFeed struct {
	store   *store.Store
	admin   *sql.DB
	appDSN  string
	peerDSN string
	schema  string
	appRole string
	peer    string
}

// newForeignRoleFeed builds the arrangement, or skips when the test run cannot:
// SQLite has no such concept, and creating roles needs a privileged connection.
func newForeignRoleFeed(t *testing.T) *foreignRoleFeed {
	t.Helper()
	adminDSN := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(adminDSN, "postgres://") && !strings.HasPrefix(adminDSN, "postgresql://") {
		t.Skip("PostgreSQL only: pg_stat_activity redaction between roles is what is under test")
	}
	admin, err := sql.Open("pgx", adminDSN)
	require.NoError(t, err, "open the privileged connection")
	// The privileged connection is itself a backend of another role, so an
	// idle one sitting in a pool would be indistinguishable from the foreign
	// connections these tests are arranging deliberately. Keeping none idle
	// means the arrangement is only ever what each test set up.
	admin.SetMaxIdleConns(0)
	t.Cleanup(func() { _ = admin.Close() })

	var mayCreateRoles bool
	require.NoError(t, admin.QueryRow(
		`SELECT rolsuper OR rolcreaterole FROM pg_roles WHERE rolname = current_user`,
	).Scan(&mayCreateRoles), "check whether this connection can create roles")
	if !mayCreateRoles {
		t.Skip("MSGVAULT_TEST_DB connects as a role that cannot CREATE ROLE, so a " +
			"second role cannot be arranged")
	}

	suffix := make([]byte, 6)
	_, err = rand.Read(suffix)
	require.NoError(t, err, "name the throwaway roles")
	tag := hex.EncodeToString(suffix)
	f := &foreignRoleFeed{
		admin:   admin,
		schema:  "mvfeed_s_" + tag,
		appRole: "mvfeed_app_" + tag,
		peer:    "mvfeed_peer_" + tag,
	}
	f.appDSN = roleDSN(t, adminDSN, f.appRole, f.schema)
	f.peerDSN = roleDSN(t, adminDSN, f.peer, f.schema)

	// Register teardown before creating anything, so a failure part-way through
	// setup still leaves the container clean.
	t.Cleanup(f.teardown)
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, f.appRole, feedProbeRolePassword),
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, f.peer, feedProbeRolePassword),
		fmt.Sprintf(`CREATE SCHEMA %s AUTHORIZATION %s`, f.schema, f.appRole),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, f.schema, f.peer),
	} {
		_, err := admin.Exec(stmt)
		require.NoErrorf(t, err, "set up the foreign-role arrangement: %s", stmt)
	}

	f.store = f.openStore(t)
	require.NoError(t, f.store.InitSchema(), "init schema as the unprivileged role")
	// The peer stands in for something else that reaches the same database. It
	// gets exactly what such a thing would have: the ability to read, and (for
	// the writer cases) to write.
	_, err = admin.Exec(fmt.Sprintf(
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO %s`, f.schema, f.peer))
	require.NoError(t, err, "grant the peer role table access")
	return f
}

// openStore opens ANOTHER store on the same database as the same unprivileged
// role. A fresh store carries a fresh dialect, which is the state a restarted
// server is in: no reading yet taken while every backend was visible.
func (f *foreignRoleFeed) openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(f.appDSN)
	require.NoError(t, err, "open the store as the unprivileged role")
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// peerConn opens a connection as the OTHER role and pins it to one backend, so
// a transaction opened on it stays on the backend the bound query will see.
func (f *foreignRoleFeed) peerConn(t *testing.T) *sql.Conn {
	t.Helper()
	db, err := sql.Open("pgx", f.peerDSN)
	require.NoError(t, err, "open a connection as the peer role")
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background())
	require.NoError(t, err, "pin the peer connection to one backend")
	t.Cleanup(func() {
		// Roll back anything still open: the schema drop that follows would
		// otherwise wait on it forever.
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
		_ = db.Close()
	})
	return conn
}

func (f *foreignRoleFeed) teardown() {
	if f.store != nil {
		_ = f.store.Close()
	}
	// Any backend still logged in as either role would block the drops.
	_, _ = f.admin.Exec(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = ANY($1)`,
		"{"+f.appRole+","+f.peer+"}")
	for _, stmt := range []string{
		fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, f.schema),
		fmt.Sprintf(`DROP OWNED BY %s CASCADE`, f.peer),
		fmt.Sprintf(`DROP OWNED BY %s CASCADE`, f.appRole),
		`DROP ROLE IF EXISTS ` + f.peer,
		`DROP ROLE IF EXISTS ` + f.appRole,
	} {
		_, _ = f.admin.Exec(stmt)
	}
}

// roleDSN rewrites the privileged DSN to connect as role, in schema.
func roleDSN(t *testing.T, adminDSN, role, schema string) string {
	t.Helper()
	u, err := url.Parse(adminDSN)
	require.NoError(t, err, "parse MSGVAULT_TEST_DB")
	u.User = url.UserPassword(role, feedProbeRolePassword)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// peerBeginWrite has the peer role stamp a watermark on a message and hold the
// transaction open, returning the stamp it wrote. The change is real and
// uncommitted: exactly the thing the bound must not publish past.
func peerBeginWrite(t *testing.T, conn *sql.Conn, id int64, subject string) time.Time {
	t.Helper()
	ctx := context.Background()
	_, err := conn.ExecContext(ctx, "BEGIN")
	require.NoError(t, err, "begin the peer's write transaction")
	_, err = conn.ExecContext(ctx,
		`UPDATE messages SET subject = $1 WHERE id = $2`, subject, id)
	require.NoError(t, err, "the peer role stamps a watermark")
	var stamp time.Time
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT content_changed_at FROM messages WHERE id = $1`, id).Scan(&stamp),
		"read the peer's uncommitted stamp")
	return stamp.UTC()
}

// TestListChangedMessages_BenignForeignConnectionsDoNotHoldTheFeedBack pins the
// fail-CLOSED direction of the redaction count.
//
// docs/api-server.md promises that only writers hold the feed back — a long
// read, an idle connection and autovacuum do not. The count that gates the
// visibility floor is what decides that, and counting every foreign backend
// instead of the ones holding a write lock on the message table turned any
// connection at all — a monitoring exporter, a backup, someone's psql — into a
// permanent freeze, recoverable only when that backend actually terminated.
func TestListChangedMessages_BenignForeignConnectionsDoNotHoldTheFeedBack(t *testing.T) {
	require := require.New(t)
	f := newForeignRoleFeed(t)
	ctx := context.Background()

	seedFeedMessage(t, f.store, 1, time.Time{})
	settleFeedClock(t, f.store)
	consumer := newChangeFeedConsumer()
	consumer.drain(t, f.store)

	// Connected and doing nothing at all: the shape of a pooled connection
	// belonging to anything else on the box.
	idle := f.peerConn(t)
	require.NoError(idle.PingContext(ctx), "the idle peer connection is live")

	// Inside a transaction, reading the message table and nothing else: a long
	// report, a pg_dump. It holds ACCESS SHARE, which cannot produce a stamp.
	reader := f.peerConn(t)
	_, err := reader.ExecContext(ctx, "BEGIN")
	require.NoError(err, "begin the peer's read transaction")
	var seen int
	require.NoError(reader.QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&seen),
		"the peer reads the message table")

	changed := seedFeedMessage(t, f.store, 2, time.Time{})
	require.True(
		consumer.drainUntil(t, f.store, func() bool { return consumer.subject(changed) != "" }),
		"a change committed while two foreign-role connections were merely connected and "+
			"reading was never delivered: neither can hold a watermark, so neither may "+
			"hold the feed back — a feed that freezes on any foreign connection freezes "+
			"permanently behind a connection pool")
}

// TestListChangedMessages_ForeignRoleWriterStillHoldsTheBound pins the
// fail-OPEN direction of the same count, which is the dangerous one.
//
// Scoping the count must not narrow it past the case it exists for: a writer
// this role cannot see. While that writer's transaction is open the feed must
// stop, and it must resume when the transaction ENDS — not when the connection
// goes away, which is what an unscoped count required and which never happens
// behind a pool.
func TestListChangedMessages_ForeignRoleWriterStillHoldsTheBound(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newForeignRoleFeed(t)

	held := seedFeedMessage(t, f.store, 1, time.Time{})
	later := seedFeedMessage(t, f.store, 2, time.Time{})
	settleFeedClock(t, f.store)
	consumer := newChangeFeedConsumer()
	consumer.drain(t, f.store)

	const byPeer, byStore = "changed by the other role", "changed by the store"
	peer := f.peerConn(t)
	stamp := peerBeginWrite(t, peer, held, byPeer)

	// The store commits a LATER change of its own. Publishing it would park the
	// consumer's cursor above the peer's stamp, which is how the row is lost.
	setSubject(t, f.store, later, byStore)

	page := consumer.mustPoll(t, f.store)
	assert.Falsef(page.CompleteThrough.After(stamp),
		"the feed claims completeness through %s while a writer this role cannot see "+
			"has stamped %s and not committed: the cursor would step over that change "+
			"and never come back to it", page.CompleteThrough, stamp)
	assert.NotEqual(byStore, consumer.subject(later),
		"a change stamped above an invisible writer's stamp must wait, not be published")

	_, err := peer.ExecContext(context.Background(), "COMMIT")
	require.NoError(err, "the peer commits")
	// The peer's CONNECTION stays open and idle from here on: recovery must
	// follow the transaction, not the connection.
	require.True(
		consumer.drainUntil(t, f.store, func() bool {
			return consumer.subject(held) == byPeer && consumer.subject(later) == byStore
		}),
		"both changes must arrive once the foreign transaction commits, even though "+
			"its connection is still open: a feed that waits for the backend to "+
			"terminate never recovers behind a pool")
}

// TestListChangedMessages_UntrustedBoundIsRefusedOnAFreshProcess pins what
// happens when there is no floor to fall back to.
//
// The floor is the last bound taken while every backend was visible. A process
// that has never taken one — a server that started while a foreign writer was
// already holding its transaction open — has nothing to cap the bound at. The
// only two answers are to publish the unfloored bound, which steps over the
// invisible writer's change and loses it permanently, or to refuse. It must
// refuse, and say what the operator has to grant, exactly as it already does
// when `messages` cannot be resolved.
func TestListChangedMessages_UntrustedBoundIsRefusedOnAFreshProcess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newForeignRoleFeed(t)

	held := seedFeedMessage(t, f.store, 1, time.Time{})
	settleFeedClock(t, f.store)

	const byPeer = "changed by the other role"
	peer := f.peerConn(t)
	stamp := peerBeginWrite(t, peer, held, byPeer)

	// A restarted server: a new store, so a new dialect with no remembered
	// reading from a time when every backend was visible.
	fresh := f.openStore(t)
	_, err := fresh.ListChangedMessages(context.Background(),
		store.ChangedMessagesFrom(stamp.Add(-time.Microsecond)), 100)
	require.Error(err,
		"with an invisible writer holding a stamp and no earlier fully-visible reading "+
			"to fall back to, there is no bound this page can trust; serving one anyway "+
			"is the row loss the bound exists to prevent")
	assert.Contains(err.Error(), "pg_read_all_stats",
		"the error must name the grant that fixes it, so an operator is not left "+
			"guessing at a feed that refuses to serve")

	_, commitErr := peer.ExecContext(context.Background(), "COMMIT")
	require.NoError(commitErr, "the peer commits")
	consumer := newChangeFeedConsumer()
	consumer.cursor = store.ChangedMessagesFrom(stamp.Add(-time.Microsecond))
	require.True(
		consumer.drainUntil(t, fresh, func() bool { return consumer.subject(held) == byPeer }),
		"refusing must be a pause, not a wedge: once the invisible writer commits the "+
			"page serves again and the change arrives")
}
