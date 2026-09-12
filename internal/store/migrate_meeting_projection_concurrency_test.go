package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
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

// These gates wrap the real database driver. They pause observable statements
// or cancel just after a real commit; production persistence remains intact.
type meetingProjectionGate struct {
	before    func(context.Context, string, []driver.NamedValue) error
	committed func(projected bool)
}

type meetingProjectionConnector struct {
	driver.Connector

	gate *meetingProjectionGate
}

func (c *meetingProjectionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &meetingProjectionConn{Conn: conn, gate: c.gate}, nil
}

type meetingProjectionConn struct {
	driver.Conn

	gate      *meetingProjectionGate
	projected bool
}

func (c *meetingProjectionConn) before(ctx context.Context, query string, args []driver.NamedValue) error {
	if c.gate.before != nil {
		return c.gate.before(ctx, strings.Join(strings.Fields(query), " "), args)
	}
	return nil
}
func (c *meetingProjectionConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.before(ctx, query, args); err != nil {
		return nil, err
	}
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return queryer.QueryContext(ctx, query, args)
}
func (c *meetingProjectionConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.before(ctx, query, args); err != nil {
		return nil, err
	}
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, query, args)
	if err == nil && strings.HasPrefix(strings.TrimSpace(query), "INSERT INTO meeting_details") {
		c.projected = true
	}
	return result, err
}
func (c *meetingProjectionConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.projected = false
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, fmt.Errorf("fixture connection %T does not support transaction options", c.Conn)
	}
	tx, err := beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &meetingProjectionTx{Tx: tx, conn: c}, nil
}
func (c *meetingProjectionConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

type meetingProjectionTx struct {
	driver.Tx

	conn *meetingProjectionConn
}

func (tx *meetingProjectionTx) Commit() error {
	err := tx.Tx.Commit()
	if err == nil && tx.conn.gate.committed != nil {
		tx.conn.gate.committed(tx.conn.projected)
	}
	return err
}
func gatedMeetingProjectionStore(t *testing.T, base *Store, gate *meetingProjectionGate) *Store {
	t.Helper()
	var connector driver.Connector
	if base.IsPostgreSQL() {
		config, err := postgresConnConfig(base.dbPath, false)
		require.NoError(t, err)
		connector = stdlib.GetConnector(*config)
	} else {
		sqliteDriver := &sqlite3.SQLiteDriver{ConnectHook: sqliteutil.RegisterFunctions}
		connector = &rfc822IDBackfillSQLiteConnector{driver: sqliteDriver, dsn: base.dbPath + testSQLiteParams}
	}
	db := sql.OpenDB(&meetingProjectionConnector{Connector: connector, gate: gate})
	db.SetMaxOpenConns(2)
	st := &Store{db: newLoggedDB(db, base.dialect.Rebind), dbPath: base.dbPath, dialect: base.dialect, fts5Available: base.fts5Available, directoryProjectionReady: base.directoryProjectionReady}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestMeetingProjectionUpgradeCancellationResumesCommittedRows(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	base := newRFC822IDBackfillBackendStore(t)
	for i := range 103 {
		projectionFixture(t, base, fmt.Sprintf("resume-%03d", i), "meeting_json", meetingProjectionRaw)
	}
	removeMeetingProjectionSchema(t, base)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	committed := 0
	gate := &meetingProjectionGate{committed: func(projected bool) {
		if projected {
			committed++
			if committed == 3 {
				cancel()
			}
		}
	}}
	st := gatedMeetingProjectionStore(t, base, gate)
	requirements.ErrorIs(st.InitSchemaContext(ctx), context.Canceled)
	var count int
	requirements.NoError(base.db.QueryRow(`SELECT COUNT(*) FROM meeting_details`).Scan(&count))
	assertions.Equal(3, count, "commits before cancellation survive")
	applied, err := base.IsMigrationAppliedContext(t.Context(), projectionMigrationName, 1)
	requirements.NoError(err)
	assertions.False(applied)
	gate.committed = nil
	requirements.NoError(st.InitSchemaContext(t.Context()))
	requirements.NoError(base.db.QueryRow(`SELECT COUNT(*) FROM meeting_details`).Scan(&count))
	assertions.Equal(103, count, "resume crosses the 100-ID page boundary")
	applied, err = base.IsMigrationAppliedContext(t.Context(), projectionMigrationName, 1)
	requirements.NoError(err)
	assertions.True(applied)
}

func TestMeetingProjectionBackfillLocksBeforeEvidenceAndSeesConcurrentUpdate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	base := newRFC822IDBackfillBackendStore(t)
	_, id := projectionFixture(t, base, "concurrent", "meeting_json", meetingProjectionRaw)
	_, err := base.db.Exec(`DELETE FROM meeting_action_items WHERE message_id = ?`, id)
	requirements.NoError(err)
	_, err = base.db.Exec(`DELETE FROM meeting_details WHERE message_id = ?`, id)
	requirements.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	lockAttempt := make(chan struct{})
	var once sync.Once
	gate := &meetingProjectionGate{before: func(_ context.Context, query string, _ []driver.NamedValue) error {
		if strings.Contains(query, "FOR UPDATE") || strings.HasPrefix(query, "UPDATE embedding_change_clock") {
			once.Do(func() { close(lockAttempt) })
		}
		return nil
	}}
	st := gatedMeetingProjectionStore(t, base, gate)
	updater, err := base.db.BeginTx(ctx, nil)
	requirements.NoError(err)
	defer func() { _ = updater.Rollback() }()
	requirements.NoError(base.lockMeetingEvidenceWith(ctx, updater, id))
	newer := `{"summary_text":"Concurrent current evidence","action_items":[{"title":"Current action","status":"done"}]}`
	requirements.NoError(upsertMessageRawWithFormat(boundQuerier{ctx: ctx, q: updater}, id, []byte(newer), "meeting_json"))
	done := make(chan error, 1)
	go func() { done <- st.backfillMeetingProjectionsContext(ctx) }()
	select {
	case <-lockAttempt:
	case <-ctx.Done():
		requirements.NoError(ctx.Err(), "backfill never reached its message/writer lock")
	}
	requirements.NoError(updater.Commit())
	select {
	case err := <-done:
		requirements.NoError(err)
	case <-ctx.Done():
		requirements.NoError(ctx.Err(), "backfill did not finish")
	}
	content, _ := readProjection(t, base, id)
	assertions.Equal("Concurrent current evidence", content.Summary.Text)
	requirements.Len(content.Actions, 1)
	assertions.Equal("Current action", content.Actions[0].Title)
	var title string
	requirements.NoError(base.db.QueryRow(`SELECT title FROM meeting_action_items WHERE message_id = ?`, id).Scan(&title))
	assertions.Equal("Current action", title)
}

func TestMeetingProjectionPublicRawWritesLockMessageBeforeRaw(t *testing.T) {
	for _, mime := range []bool{false, true} {
		t.Run(fmt.Sprint("mime=", mime), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			base := newRFC822IDBackfillBackendStore(t)
			_, id := projectionFixture(t, base, "raw-lock", "meeting_json", meetingProjectionRaw)
			statement := make(chan string, 1)
			var once sync.Once
			gate := &meetingProjectionGate{before: func(_ context.Context, query string, _ []driver.NamedValue) error {
				once.Do(func() { statement <- query })
				return nil
			}}
			st := gatedMeetingProjectionStore(t, base, gate)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			held, err := base.db.BeginTx(ctx, nil)
			requirements.NoError(err)
			defer func() { _ = held.Rollback() }()
			requirements.NoError(base.lockMeetingEvidenceWith(ctx, held, id))
			done := make(chan error, 1)
			go func() {
				if mime {
					done <- st.UpsertMessageRaw(id, []byte("MIME changed"))
				} else {
					done <- st.UpsertMessageRawWithFormat(id, []byte(`{"action_items":[]}`), "meeting_json")
				}
			}()
			select {
			case first := <-statement:
				if base.IsPostgreSQL() {
					assertions.Contains(first, "FROM messages WHERE id =")
					assertions.Contains(first, "FOR UPDATE")
				} else {
					assertions.Contains(first, "UPDATE embedding_change_clock")
				}
			case <-ctx.Done():
				requirements.NoError(ctx.Err(), "raw write did not attempt its first statement")
			}
			requirements.NoError(held.Commit())
			select {
			case err := <-done:
				requirements.NoError(err)
			case <-ctx.Done():
				requirements.NoError(ctx.Err(), "raw write did not finish")
			}
			content, _ := readProjection(t, base, id)
			assertions.Empty(content.Actions)
		})
	}
}
