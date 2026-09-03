package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// gmailAuditSnapshotGateKey arms the driver-boundary gate for exactly one
// page load. Queries issued with a context carrying the gate pause before the
// first statement that binds the target message ID executes — the point where
// the page load is about to touch the second record's rows.
type gmailAuditSnapshotGateKey struct{}

type gmailAuditSnapshotGate struct {
	targetID    int64
	pauseOnce   sync.Once
	releaseOnce sync.Once
	paused      chan struct{}
	release     chan struct{}
}

func newGmailAuditSnapshotGate(targetID int64) *gmailAuditSnapshotGate {
	return &gmailAuditSnapshotGate{
		targetID: targetID,
		paused:   make(chan struct{}),
		release:  make(chan struct{}),
	}
}

// hold blocks before the gated statement executes. Only the first matching
// statement pauses; later ones run through so the page load can finish.
func (g *gmailAuditSnapshotGate) hold(ctx context.Context) error {
	var waiting bool
	g.pauseOnce.Do(func() {
		waiting = true
		close(g.paused)
	})
	if !waiting {
		return nil
	}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gmailAuditSnapshotGate) resume() {
	g.releaseOnce.Do(func() { close(g.release) })
}

// gatesTargetQuery reports whether the statement binds the target message ID.
// A joined page query binds every page ID and a per-record query binds the
// one record it loads, so both shapes of the evidence load are observable
// with a single matcher.
func (g *gmailAuditSnapshotGate) gatesTargetQuery(args []driver.NamedValue) bool {
	for _, arg := range args {
		if value, ok := arg.Value.(int64); ok && value == g.targetID {
			return true
		}
	}
	return false
}

type gmailAuditSnapshotConnector struct {
	driver.Connector

	gate *gmailAuditSnapshotGate
}

func (c *gmailAuditSnapshotConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &gmailAuditSnapshotConn{Conn: conn, gate: c.gate}, nil
}

type gmailAuditSnapshotConn struct {
	driver.Conn

	gate *gmailAuditSnapshotGate
}

func (c *gmailAuditSnapshotConn) QueryContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if gate, armed := ctx.Value(gmailAuditSnapshotGateKey{}).(*gmailAuditSnapshotGate); armed &&
		gate == c.gate && gate.gatesTargetQuery(args) {
		if err := gate.hold(ctx); err != nil {
			return nil, err
		}
	}
	return queryer.QueryContext(ctx, query, args)
}

func (c *gmailAuditSnapshotConn) ExecContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Result, error) {
	if execer, ok := c.Conn.(driver.ExecerContext); ok {
		return execer.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *gmailAuditSnapshotConn) PrepareContext(
	ctx context.Context, query string,
) (driver.Stmt, error) {
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Prepare(query)
}

func (c *gmailAuditSnapshotConn) BeginTx(
	ctx context.Context, opts driver.TxOptions,
) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	return nil, errors.New("wrapped Gmail audit snapshot connection does not implement ConnBeginTx")
}

func (c *gmailAuditSnapshotConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *gmailAuditSnapshotConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *gmailAuditSnapshotConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *gmailAuditSnapshotConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

type gmailAuditSnapshotSQLiteConnector struct {
	driver driver.Driver
	dsn    string
}

func (c *gmailAuditSnapshotSQLiteConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.dsn)
}

func (c *gmailAuditSnapshotSQLiteConnector) Driver() driver.Driver { return c.driver }

func newGmailAuditSnapshotSQLiteStore(
	t *testing.T, gate *gmailAuditSnapshotGate,
) *Store {
	t.Helper()
	require := require.New(t)
	sqliteDriver := &sqlite3.SQLiteDriver{ConnectHook: func(conn *sqlite3.SQLiteConn) error {
		return conn.RegisterFunc(sqliteutil.UnicodeLowerFunction, strings.ToLower, true)
	}}
	base := &gmailAuditSnapshotSQLiteConnector{
		driver: sqliteDriver,
		dsn:    filepath.Join(t.TempDir(), "gmail-audit-snapshot.db") + testSQLiteParams,
	}
	db := sql.OpenDB(&gmailAuditSnapshotConnector{Connector: base, gate: gate})
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	dialect := &SQLiteDialect{}
	st := &Store{db: newLoggedDB(db, dialect.Rebind), dbPath: base.dsn, dialect: dialect}
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchemaContext(t.Context()))
	return st
}

func newGmailAuditSnapshotPostgresStore(
	t *testing.T, dbURL string, gate *gmailAuditSnapshotGate,
) *Store {
	t.Helper()
	require := require.New(t)
	schemaName := fmt.Sprintf("msgvault_test_gmail_audit_snapshot_%d", time.Now().UnixNano())
	admin, err := sql.Open("pgx", dbURL)
	require.NoError(err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(t.Context(), "CREATE SCHEMA "+schemaName)
	require.NoError(err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schemaName+" CASCADE")
	})

	separator := "?"
	if strings.Contains(dbURL, "?") {
		separator = "&"
	}
	testURL := dbURL + separator + "search_path=" + schemaName
	config, err := postgresConnConfig(testURL, false)
	require.NoError(err)
	db := sql.OpenDB(&gmailAuditSnapshotConnector{
		Connector: stdlib.GetConnector(*config), gate: gate,
	})
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	dialect := &PostgreSQLDialect{}
	require.NoError(dialect.InitConn(db))
	st := &Store{db: newLoggedDB(db, dialect.Rebind), dbPath: testURL, dialect: dialect}
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchemaContext(t.Context()))
	return st
}

func seedGmailAuditSnapshotMessage(
	t *testing.T, st *Store, sourceID int64, sourceMessageID, fromAddress string,
) int64 {
	t.Helper()
	require := require.New(t)
	conversationID, err := st.EnsureConversation(sourceID, "thread-"+sourceMessageID, sourceMessageID)
	require.NoError(err)
	participantID, err := st.EnsureParticipant(fromAddress, "Snapshot Sender", "example.test")
	require.NoError(err)
	id, err := st.PersistMessage(&MessagePersistData{
		Message: &Message{
			ConversationID:  conversationID,
			SourceID:        sourceID,
			SourceMessageID: sourceMessageID,
			MessageType:     MessageTypeEmail,
			Subject:         sql.NullString{String: sourceMessageID, Valid: true},
			SenderID:        sql.NullInt64{Int64: participantID, Valid: true},
		},
		RawMIME: []byte("raw-" + sourceMessageID),
		Recipients: []RecipientSet{{
			Type: "from", ParticipantIDs: []int64{participantID},
			EmailAddresses: []string{fromAddress},
		}},
	})
	require.NoError(err)
	return id
}

func gmailAuditRecipientAddress(evidence GmailAuditEvidence, recipientType string) string {
	for _, recipient := range evidence.Recipients {
		if recipient.Type == recipientType && recipient.EmailAddress.Valid {
			return recipient.EmailAddress.String
		}
	}
	return ""
}

// TestGmailAuditEvidencePageReadsOneCoherentSnapshot observes the production
// page load at the real driver boundary (SQLite here; the same test runs on
// PostgreSQL when MSGVAULT_TEST_DB points at one). The gate pauses the page
// load right before the first statement touching the second record's rows;
// a concurrent repair commit lands in that window. One page must then read
// every record from the snapshot its first statement established — a
// committed rewrite must not split the page across two database states.
func TestGmailAuditEvidencePageReadsOneCoherentSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	gate := newGmailAuditSnapshotGate(0)
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	var st *Store
	if IsPostgresURL(dbURL) {
		st = newGmailAuditSnapshotPostgresStore(t, dbURL, gate)
	} else {
		st = newGmailAuditSnapshotSQLiteStore(t, gate)
	}
	source, err := st.GetOrCreateSource("gmail", "snapshot-audit@example.test")
	require.NoError(err)
	firstID := seedGmailAuditSnapshotMessage(t, st, source.ID, "snapshot-a", "first-sender@example.test")
	secondID := seedGmailAuditSnapshotMessage(t, st, source.ID, "snapshot-b", "second-sender@example.test")
	require.Less(firstID, secondID)
	// Arm the pre-created gate only now: the target ID is read by the gated
	// goroutine, which has not started yet.
	gate.targetID = secondID

	type pageResult struct {
		evidence []GmailAuditEvidence
		pageSize int
		err      error
	}
	done := make(chan pageResult, 1)
	ctx, cancel := context.WithTimeout(
		context.WithValue(t.Context(), gmailAuditSnapshotGateKey{}, gate), 15*time.Second,
	)
	defer cancel()
	var delivered []GmailAuditEvidence
	go func() {
		pageSize, pageErr := st.StreamGmailAuditEvidencePageContext(
			ctx, source.ID, 0, 100, func(record GmailAuditEvidence) error {
				delivered = append(delivered, record)
				return nil
			})
		done <- pageResult{evidence: delivered, pageSize: pageSize, err: pageErr}
	}()

	select {
	case <-gate.paused:
	case <-time.After(10 * time.Second):
		require.FailNow("page load never reached the second record's rows")
	}
	// The page had to touch the second record's rows, so the first record
	// must already have been delivered to the audit consumer and released —
	// one page never materializes more than one message's evidence.
	require.Len(delivered, 1,
		"each evidence record must be delivered before the next record's rows load")
	assert.Equal(firstID, delivered[0].ID)

	// The concurrent-repair stand-in: rewrite the second message's envelope
	// while the page load is between its records, then commit on another
	// connection.
	_, err = st.DB().ExecContext( //nolint:gosec // The SQL is static; Rebind only changes placeholders.
		t.Context(), st.Rebind(`
		UPDATE message_recipients SET email_address = 'rewritten-sender@example.test'
		WHERE message_id = ? AND recipient_type = 'from'
	`), secondID)
	require.NoError(err)
	gate.resume()

	var page pageResult
	select {
	case page = <-done:
	case <-time.After(10 * time.Second):
		require.FailNow("page load did not finish after the concurrent commit")
	}
	require.NoError(page.err)
	require.Equal(2, page.pageSize,
		"the keyset page size must count selected IDs so pagination stays exact")
	require.Len(page.evidence, 2)
	assert.Equal(firstID, page.evidence[0].ID)
	assert.Equal(secondID, page.evidence[1].ID)
	assert.Equal("second-sender@example.test", gmailAuditRecipientAddress(page.evidence[1], "from"),
		"one page must read every record from one snapshot; a concurrently committed rewrite must not split it")

	// The rewrite really committed: a fresh read outside the page observes it,
	// so the assertion above proves snapshot coherence, not a lost write.
	var committed string
	require.NoError(st.DB().QueryRowContext( //nolint:gosec // The SQL is static; Rebind only changes placeholders.
		t.Context(), st.Rebind(`
		SELECT email_address FROM message_recipients
		WHERE message_id = ? AND recipient_type = 'from'
	`), secondID).Scan(&committed))
	assert.Equal("rewritten-sender@example.test", committed)
}
