package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// The connector holds the SQLite writer lock immediately after the identity
// transaction commits, before its caller can start the attribution pass.
type identityCommitConnector struct {
	driver.Connector

	afterCommit func() error
	afterBusy   func() error
	released    chan error
	armed       atomic.Bool
}

func (c *identityCommitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	sqliteConn, ok := conn.(*sqlite3.SQLiteConn)
	if !ok {
		return nil, fmt.Errorf("unexpected sqlite connection type %T", conn)
	}
	return &identityCommitConn{SQLiteConn: sqliteConn, gate: c}, nil
}

type identityCommitConn struct {
	*sqlite3.SQLiteConn

	gate *identityCommitConnector
}

func (c *identityCommitConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.SQLiteConn.ExecContext(ctx, query, args)
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) && (sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked) && c.gate.afterBusy != nil {
		c.gate.released <- c.gate.afterBusy()
		c.gate.afterBusy = nil
	}
	if err != nil {
		return nil, fmt.Errorf("execute statement: %w", err)
	}
	return result, nil
}

func (c *identityCommitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.SQLiteConn.BeginTx(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	return &identityCommitTx{Tx: tx, gate: c.gate}, nil
}

type identityCommitTx struct {
	driver.Tx

	gate *identityCommitConnector
}

func (tx *identityCommitTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.gate.armed.Swap(false) {
		return tx.gate.afterCommit()
	}
	return nil
}

func TestConfirmIdentityRetriesAccountAttribution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	s, err := OpenForTest(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, s.Close()) })
	require.NoError(t, s.InitSchema())
	source, err := s.GetOrCreateSource("gmail", "inbox@example.com")
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(1,?,'thread','email_thread'); INSERT INTO messages(id,source_id,source_message_id,conversation_id,message_type,account_address,account_path) VALUES(1,?,'one',1,'email','work@example.org','inbound')`, source.ID, source.ID)
	require.NoError(t, err)
	blocker, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, blocker.Close()) })
	conn, err := blocker.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	released := make(chan error, 1)
	base := &gmailAuditSnapshotSQLiteConnector{driver: &sqlite3.SQLiteDriver{ConnectHook: sqliteutil.RegisterFunctions}, dsn: path + "?_journal_mode=WAL&_busy_timeout=0&_foreign_keys=ON"}
	gate := &identityCommitConnector{Connector: base, released: released}
	gate.afterCommit = func() error {
		_, err := conn.ExecContext(t.Context(), `BEGIN IMMEDIATE`)
		return err
	}
	gate.afterBusy = func() error {
		_, err := conn.ExecContext(t.Context(), `ROLLBACK`)
		return err
	}

	db := sql.OpenDB(gate)
	db.SetMaxOpenConns(1)
	replaceTestStoreDB(t, s, db)
	gate.armed.Store(true)
	require.NoError(t, s.AddAccountIdentityContext(t.Context(), source.ID, "work@example.org", "manual"))
	select {
	case err := <-released:
		require.NoError(t, err)
	default:
		require.FailNow(t, "the attribution pass did not encounter the held lock")
	}
	var pending int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE account_path IS NULL`).Scan(&pending))
	assert.Zero(t, pending)
}

func TestRepairAccountAttributionRetriesBusyPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	s, err := OpenForTest(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, s.Close()) })
	require.NoError(t, s.InitSchema())
	source, err := s.GetOrCreateSource("gmail", "inbox@example.com")
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(1,?,'thread','email_thread'); INSERT INTO messages(id,source_id,source_message_id,conversation_id,message_type) VALUES(1,?,'one',1,'email'),(2,?,'two',1,'email')`, source.ID, source.ID, source.ID)
	require.NoError(t, err)
	blocker, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, blocker.Close()) })
	conn, err := blocker.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	released := make(chan error, 1)
	base := &gmailAuditSnapshotSQLiteConnector{driver: &sqlite3.SQLiteDriver{ConnectHook: sqliteutil.RegisterFunctions}, dsn: path + "?_journal_mode=WAL&_busy_timeout=0&_foreign_keys=ON"}
	gate := &identityCommitConnector{Connector: base, released: released}
	gate.afterCommit = func() error {
		_, err := conn.ExecContext(t.Context(), `BEGIN IMMEDIATE`)
		return err
	}
	gate.afterBusy = func() error {
		_, err := conn.ExecContext(t.Context(), `ROLLBACK`)
		return err
	}
	db := sql.OpenDB(gate)
	db.SetMaxOpenConns(1)
	replaceTestStoreDB(t, s, db)
	s.accountRepairPageSizeOverride = 1
	gate.armed.Store(true)
	var progress []int64
	summary, err := s.RepairAccountAttributionContext(t.Context(), source.ID, func(result AccountAttributionRepairSummary) {
		progress = append(progress, result.Scanned)
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), summary.Scanned)
	assert.Equal(t, []int64{1, 2}, progress)
	select {
	case err := <-released:
		require.NoError(t, err)
	default:
		require.FailNow(t, "the second page did not encounter the held lock")
	}
}

func TestIdentityCommitLeavesPendingWhenRequestCanceled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	s, err := OpenForTest(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, s.Close()) })
	require.NoError(t, s.InitSchema())
	source, err := s.GetOrCreateSource("gmail", "inbox@example.com")
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(1,?,'thread','email_thread'); INSERT INTO messages(id,source_id,source_message_id,conversation_id,message_type,account_address,account_path) VALUES(1,?,'one',1,'email','work@example.org','inbound')`, source.ID, source.ID)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	base := &gmailAuditSnapshotSQLiteConnector{driver: &sqlite3.SQLiteDriver{ConnectHook: sqliteutil.RegisterFunctions}, dsn: path + "?_journal_mode=WAL&_busy_timeout=0&_foreign_keys=ON"}
	gate := &identityCommitConnector{Connector: base, released: make(chan error, 1), afterCommit: func() error { cancel(); return nil }}
	db := sql.OpenDB(gate)
	db.SetMaxOpenConns(1)
	replaceTestStoreDB(t, s, db)
	gate.armed.Store(true)
	require.NoError(t, s.AddAccountIdentityContext(ctx, source.ID, "work@example.org", "manual"))
	pending, err := s.PendingAccountAttributionSourcesContext(t.Context())
	require.NoError(t, err)
	assert.Contains(t, pending, source.ID)
	_, err = s.RepairAccountAttributionContext(t.Context(), source.ID, nil)
	require.NoError(t, err)
	pending, err = s.PendingAccountAttributionSourcesContext(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, pending, source.ID)
}
