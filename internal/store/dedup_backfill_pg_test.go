package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

type rfc822IDBackfillApplyContextKey struct{}

func TestStore_RFC822IDBackfillStreamsAcrossBoundedPages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	st.rfc822IDBackfillBatchSizeOverride = 2

	source, err := st.GetOrCreateSource("gmail", "bounded-backfill@example.test")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(
		source.ID, "bounded-backfill-conversation", "bounded backfill")
	require.NoError(err)
	messageIDs := make([]int64, 5)
	for i := range messageIDs {
		messageIDs[i], err = st.UpsertMessage(&Message{
			ConversationID: conversationID,
			SourceID:       source.ID,
			SourceMessageID: fmt.Sprintf(
				"bounded-backfill-%d", i),
			MessageType: "email",
		})
		require.NoError(err)
		require.NoError(st.UpsertMessageRaw(messageIDs[i], []byte(fmt.Sprintf(
			"Message-ID: <bounded-backfill-%d@example.test>\r\n\r\nBody", i))))
	}

	plan, err := st.PlanRFC822IDBackfill(t.Context(), []int64{source.ID})
	require.NoError(err)
	assert.Equal(int64(5), plan.Candidates)
	assert.Equal(int64(5), plan.Ready)
	assert.Equal(int64(0), plan.Failed)
	assert.NotEmpty(plan.Digest())
	st.rfc822IDBackfillBatchSizeOverride = 3
	repagedPlan, err := st.PlanRFC822IDBackfill(t.Context(), []int64{source.ID})
	require.NoError(err)
	assert.Equal(plan.Digest(), repagedPlan.Digest(),
		"digest must not depend on bounded page boundaries")
	st.rfc822IDBackfillBatchSizeOverride = 2

	updated, err := st.ApplyRFC822IDBackfill(
		t.Context(), []int64{source.ID}, plan, nil)
	require.NoError(err)
	assert.Equal(int64(5), updated)
	for i, messageID := range messageIDs {
		assert.Equal(fmt.Sprintf("bounded-backfill-%d@example.test", i),
			internalStoredRFC822ID(t, st, messageID))
	}
}

func newRFC822IDBackfillBackendStore(t *testing.T) *Store {
	t.Helper()
	if dbURL := os.Getenv("MSGVAULT_TEST_DB"); IsPostgresURL(dbURL) {
		return newPGStoreInternal(t, dbURL)
	}
	st, err := OpenForTest(filepath.Join(t.TempDir(), "bounded-rfc822-backfill.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchemaContext(t.Context()))
	return st
}

type rfc822IDBackfillStatementGate struct {
	statements chan string
}

func (g *rfc822IDBackfillStatementGate) report(ctx context.Context, query string) error {
	if active, _ := ctx.Value(rfc822IDBackfillApplyContextKey{}).(bool); !active {
		return nil
	}
	normalized := strings.ToUpper(strings.Join(strings.Fields(query), " "))
	select {
	case g.statements <- normalized:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type rfc822IDBackfillGateConnector struct {
	driver.Connector

	statementGate *rfc822IDBackfillStatementGate
	rowGate       *rfc822IDBackfillPostgresRowGate
	connections   atomic.Int64
}

func (c *rfc822IDBackfillGateConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &rfc822IDBackfillGateConn{
		Conn: conn, statementGate: c.statementGate, rowGate: c.rowGate,
		report: c.connections.Add(1) == 2,
	}, nil
}

type rfc822IDBackfillSQLiteConnector struct {
	driver driver.Driver
	dsn    string
}

func (c *rfc822IDBackfillSQLiteConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.dsn)
}

func (c *rfc822IDBackfillSQLiteConnector) Driver() driver.Driver { return c.driver }

type rfc822IDBackfillGateConn struct {
	driver.Conn

	statementGate *rfc822IDBackfillStatementGate
	rowGate       *rfc822IDBackfillPostgresRowGate
	report        bool
}

func (c *rfc822IDBackfillGateConn) QueryContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	if c.report && c.statementGate != nil {
		if err := c.statementGate.report(ctx, query); err != nil {
			return nil, err
		}
	}
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if c.report && c.rowGate != nil &&
		strings.Contains(strings.ToUpper(query), "FROM MESSAGES M") &&
		strings.Contains(strings.ToUpper(query), "JOIN MESSAGE_RAW MR") {
		return &rfc822IDBackfillPostgresGateRows{
			Rows: rows, ctx: ctx, gate: c.rowGate,
		}, nil
	}
	return rows, nil
}

func (c *rfc822IDBackfillGateConn) ExecContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Result, error) {
	if c.report && c.statementGate != nil {
		if err := c.statementGate.report(ctx, query); err != nil {
			return nil, err
		}
	}
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, query, args)
}

func (c *rfc822IDBackfillGateConn) PrepareContext(
	ctx context.Context, query string,
) (driver.Stmt, error) {
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Prepare(query)
}

func (c *rfc822IDBackfillGateConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *rfc822IDBackfillGateConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *rfc822IDBackfillGateConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *rfc822IDBackfillGateConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

type rfc822IDBackfillPostgresRowGate struct {
	firstRowLocked chan struct{}
	releaseFirst   chan struct{}
	lockedOnce     sync.Once
	releaseOnce    sync.Once
}

func newRFC822IDBackfillPostgresRowGate() *rfc822IDBackfillPostgresRowGate {
	return &rfc822IDBackfillPostgresRowGate{
		firstRowLocked: make(chan struct{}),
		releaseFirst:   make(chan struct{}),
	}
}

func (g *rfc822IDBackfillPostgresRowGate) pauseAfterFirstRow(ctx context.Context) error {
	pause := false
	g.lockedOnce.Do(func() {
		close(g.firstRowLocked)
		pause = true
	})
	if !pause {
		return nil
	}
	select {
	case <-g.releaseFirst:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *rfc822IDBackfillPostgresRowGate) release() {
	g.releaseOnce.Do(func() { close(g.releaseFirst) })
}

type rfc822IDBackfillPostgresGateRows struct {
	driver.Rows

	ctx  context.Context
	gate *rfc822IDBackfillPostgresRowGate
}

func (r *rfc822IDBackfillPostgresGateRows) Next(values []driver.Value) error {
	if err := r.Rows.Next(values); err != nil {
		return err
	}
	return r.gate.pauseAfterFirstRow(r.ctx)
}

func TestStore_ApplyRFC822IDBackfillSQLiteReservesWriterBeforeValidation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite lock-order contract")
	}

	gate := &rfc822IDBackfillStatementGate{statements: make(chan string, 16)}
	st := newRFC822IDBackfillSQLiteGateStore(t, gate)
	firstID, secondID, sourceID, plan := newInternalRFC822IDBackfillPlan(t, st, "sqlite-lock")

	connA, err := st.db.Conn(t.Context())
	require.NoError(err)
	t.Cleanup(func() { _ = connA.Close() })
	require.NoError(execManualSQL(t.Context(), connA, "BEGIN IMMEDIATE"))
	committedA := false
	t.Cleanup(func() {
		if !committedA {
			_ = execManualSQL(context.Background(), connA, "ROLLBACK")
		}
	})
	_, err = connA.ExecContext(t.Context(), st.dialect.Rebind(
		`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), "stale@example.com", secondID)
	require.NoError(err)

	type applyResult struct {
		updated int64
		err     error
	}
	applyCtx, cancel := context.WithTimeout(
		context.WithValue(t.Context(), rfc822IDBackfillApplyContextKey{}, true), 10*time.Second)
	defer cancel()
	resultCh := make(chan applyResult, 1)
	progressCalls := make(chan struct{}, 1)
	go func() {
		updated, applyErr := st.ApplyRFC822IDBackfill(
			applyCtx, []int64{sourceID}, plan,
			func(_, _ int64) { progressCalls <- struct{}{} },
		)
		resultCh <- applyResult{updated: updated, err: applyErr}
	}()

	select {
	case firstStatement := <-gate.statements:
		require.Equal("BEGIN IMMEDIATE", firstStatement,
			"Apply must reserve the SQLite writer before validation")
	case <-applyCtx.Done():
		require.NoError(applyCtx.Err(), "Apply did not issue its first statement")
	}
	require.NoError(execManualSQL(t.Context(), connA, "COMMIT"))
	committedA = true

	select {
	case result := <-resultCh:
		require.NoError(result.err)
		assert.Equal(int64(0), result.updated)
	case <-applyCtx.Done():
		require.NoError(applyCtx.Err(), "Apply did not finish after connection A committed")
	}
	select {
	case <-progressCalls:
		require.Fail("progress fired for a rolled-back apply")
	default:
	}
	assert.Empty(internalStoredRFC822ID(t, st, firstID))
	assert.Equal("stale@example.com", internalStoredRFC822ID(t, st, secondID))
}

func TestStore_ApplyRFC822IDBackfillPostgresLocksAscendingAndRollsBackDrift(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := skipUnlessPostgresInternal(t)
	gate := newRFC822IDBackfillPostgresRowGate()
	defer gate.release()
	st := newRFC822IDBackfillPostgresGateStore(t, dbURL, gate)
	firstID, secondID, sourceID, plan := newInternalRFC822IDBackfillPlan(t, st, "pg-lock")

	connA, err := st.db.Conn(t.Context())
	require.NoError(err)
	t.Cleanup(func() { _ = connA.Close() })
	require.NoError(execManualSQL(t.Context(), connA, "BEGIN"))
	committedA := false
	t.Cleanup(func() {
		if !committedA {
			_ = execManualSQL(context.Background(), connA, "ROLLBACK")
		}
	})
	var lockedID int64
	require.NoError(connA.QueryRowContext(t.Context(), st.dialect.Rebind(
		`SELECT id FROM messages WHERE id = ? FOR UPDATE`), secondID).Scan(&lockedID))
	require.Equal(secondID, lockedID)
	require.NoError(connA.QueryRowContext(t.Context(), st.dialect.Rebind(
		`SELECT message_id FROM message_raw WHERE message_id = ? FOR UPDATE`), secondID).Scan(&lockedID))

	type applyResult struct {
		updated int64
		err     error
	}
	applyCtx, cancel := context.WithTimeout(
		context.WithValue(t.Context(), rfc822IDBackfillApplyContextKey{}, true), 30*time.Second)
	defer cancel()
	resultCh := make(chan applyResult, 1)
	progressCalls := make(chan struct{}, 1)
	go func() {
		updated, applyErr := st.ApplyRFC822IDBackfill(
			applyCtx, []int64{sourceID}, plan,
			func(_, _ int64) { progressCalls <- struct{}{} },
		)
		resultCh <- applyResult{updated: updated, err: applyErr}
	}()

	select {
	case <-gate.firstRowLocked:
	case <-applyCtx.Done():
		require.NoError(applyCtx.Err(), "Apply did not lock the first row")
	}
	assertPostgresRowLocked(t, connA,
		`SELECT id FROM messages WHERE id = $1 FOR UPDATE NOWAIT`, firstID)
	assertPostgresRowLocked(t, connA,
		`SELECT message_id FROM message_raw WHERE message_id = $1 FOR UPDATE NOWAIT`, firstID)
	gate.release()

	_, err = connA.ExecContext(t.Context(), st.dialect.Rebind(
		`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), "stale@example.com", secondID)
	require.NoError(err)
	require.NoError(execManualSQL(t.Context(), connA, "COMMIT"))
	committedA = true

	select {
	case result := <-resultCh:
		require.NoError(result.err)
		assert.Equal(int64(0), result.updated)
	case <-applyCtx.Done():
		require.NoError(applyCtx.Err(), "Apply did not finish after connection A committed")
	}
	select {
	case <-progressCalls:
		require.Fail("progress fired for a rolled-back apply")
	default:
	}
	assert.Empty(internalStoredRFC822ID(t, st, firstID))
	assert.Equal("stale@example.com", internalStoredRFC822ID(t, st, secondID))
}

func newRFC822IDBackfillPostgresGateStore(
	t *testing.T, dbURL string, gate *rfc822IDBackfillPostgresRowGate,
) *Store {
	t.Helper()
	base := newPGStoreInternal(t, dbURL)
	config, err := postgresConnConfig(base.dbPath, false)
	require.NoError(t, err)
	db := sql.OpenDB(&rfc822IDBackfillGateConnector{
		Connector: stdlib.GetConnector(*config), rowGate: gate,
	})
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	dialect := &PostgreSQLDialect{}
	st := &Store{db: newLoggedDB(db, dialect.Rebind), dbPath: base.dbPath, dialect: dialect}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newRFC822IDBackfillSQLiteGateStore(
	t *testing.T, gate *rfc822IDBackfillStatementGate,
) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rfc822-id-backfill-lock-order.db")
	setup, err := OpenForTest(path)
	require.NoError(t, err)
	require.NoError(t, setup.InitSchemaContext(t.Context()))
	require.NoError(t, setup.Close())

	sqliteDriver := &sqlite3.SQLiteDriver{ConnectHook: func(conn *sqlite3.SQLiteConn) error {
		return conn.RegisterFunc(sqliteutil.UnicodeLowerFunction, strings.ToLower, true)
	}}
	base := &rfc822IDBackfillSQLiteConnector{driver: sqliteDriver, dsn: path + testSQLiteParams}
	db := sql.OpenDB(&rfc822IDBackfillGateConnector{
		Connector: base, statementGate: gate,
	})
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	dialect := &SQLiteDialect{}
	st := &Store{db: newLoggedDB(db, dialect.Rebind), dbPath: base.dsn, dialect: dialect}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newInternalRFC822IDBackfillPlan(
	t *testing.T, st *Store, prefix string,
) (firstID, secondID, sourceID int64, plan RFC822IDBackfillPlan) {
	t.Helper()
	source, err := st.GetOrCreateSource("gmail", prefix+"@example.com")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversation(source.ID, prefix+"-conversation", prefix)
	require.NoError(t, err)
	newMessage := func(suffix string) int64 {
		messageID, insertErr := st.UpsertMessage(&Message{
			ConversationID: conversationID, SourceID: source.ID,
			SourceMessageID: prefix + "-" + suffix, MessageType: "email",
		})
		require.NoError(t, insertErr)
		require.NoError(t, st.UpsertMessageRaw(messageID, []byte(fmt.Sprintf(
			"Message-ID: <%s-%s@example.com>\r\n\r\n%s", prefix, suffix, suffix))))
		return messageID
	}
	firstID = newMessage("first")
	secondID = newMessage("second")
	plan, err = st.PlanRFC822IDBackfill(t.Context(), []int64{source.ID})
	require.NoError(t, err)
	require.Equal(t, int64(2), plan.Ready)
	return firstID, secondID, source.ID, plan
}

func internalStoredRFC822ID(t *testing.T, st *Store, messageID int64) string {
	t.Helper()
	var value sql.NullString
	require.NoError(t, st.db.QueryRowContext(t.Context(), st.dialect.Rebind(
		`SELECT rfc822_message_id FROM messages WHERE id = ?`), messageID).Scan(&value))
	return value.String
}

func execManualSQL(ctx context.Context, conn *sql.Conn, statement string) error {
	_, err := conn.ExecContext(ctx, statement)
	return err
}

func assertPostgresRowLocked(t *testing.T, conn *sql.Conn, query string, messageID int64) {
	t.Helper()
	require.NoError(t, execManualSQL(t.Context(), conn, "SAVEPOINT rfc822_id_backfill_probe"))
	var id int64
	err := conn.QueryRowContext(t.Context(), query, messageID).Scan(&id)
	var pgErr *pgconn.PgError
	require.Error(t, err)
	require.ErrorAs(t, err, &pgErr, "expected PostgreSQL lock error, got %T: %v", err, err)
	assert.Equal(t, "55P03", pgErr.Code)
	require.NoError(t, execManualSQL(t.Context(), conn, "ROLLBACK TO SAVEPOINT rfc822_id_backfill_probe"))
}

var _ driver.Connector = (*rfc822IDBackfillGateConnector)(nil)
var _ driver.Connector = (*rfc822IDBackfillSQLiteConnector)(nil)
var _ driver.QueryerContext = (*rfc822IDBackfillGateConn)(nil)
var _ driver.ExecerContext = (*rfc822IDBackfillGateConn)(nil)
var _ driver.Rows = (*rfc822IDBackfillPostgresGateRows)(nil)
