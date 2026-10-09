//go:build cgo

// Package sqlobserve observes statements while delegating to the real SQLite
// driver. It is intended for integration tests of database work and lifetime.
package sqlobserve

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

var driverSequence atomic.Uint64

// Observer records real SQLite driver calls in execution order.
type Observer struct {
	mu         sync.Mutex
	statements []string
	// BeforeQuery can coordinate a test at a real driver call boundary.
	BeforeQuery func(context.Context, string)
}

func (o *Observer) record(ctx context.Context, statement string) {
	o.mu.Lock()
	o.statements = append(o.statements, statement)
	o.mu.Unlock()
	if o.BeforeQuery != nil {
		o.BeforeQuery(ctx, statement)
	}
}

// Reset discards statements recorded during fixture setup.
func (o *Observer) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.statements = nil
}

// Statements returns a copy of the recorded SQL statements.
func (o *Observer) Statements() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.statements...)
}

// Open creates an observed in-memory database with the production functions.
func Open(tb testing.TB, hook func(*sqlite3.SQLiteConn) error) (*sql.DB, *Observer) {
	tb.Helper()
	observer := &Observer{}
	name := fmt.Sprintf("observed_sqlite_%d", driverSequence.Add(1))
	sql.Register(name, &observedDriver{base: &sqlite3.SQLiteDriver{ConnectHook: func(conn *sqlite3.SQLiteConn) error {
		if err := sqliteutil.RegisterFunctions(conn); err != nil {
			return err
		}
		if hook != nil {
			return hook(conn)
		}
		return nil
	}}, observer: observer})
	db, err := sql.Open(name, ":memory:")
	require.NoError(tb, err)
	db.SetMaxOpenConns(1)
	tb.Cleanup(func() { require.NoError(tb, db.Close()) })
	return db, observer
}

type observedDriver struct {
	base     *sqlite3.SQLiteDriver
	observer *Observer
}

func (d *observedDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, err //nolint:wrapcheck // Preserve the real driver's typed connection errors.
	}
	sqliteConn, ok := conn.(*sqlite3.SQLiteConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("observed SQLite driver returned %T", conn)
	}
	return &observedConn{SQLiteConn: sqliteConn, observer: d.observer}, nil
}

type observedConn struct {
	*sqlite3.SQLiteConn

	observer *Observer
}

func (c *observedConn) QueryContext(ctx context.Context, statement string, args []driver.NamedValue) (driver.Rows, error) {
	c.observer.record(ctx, statement)
	return c.SQLiteConn.QueryContext(ctx, statement, args) //nolint:wrapcheck // Preserve database/sql driver sentinels and typed errors.
}
func (c *observedConn) ExecContext(ctx context.Context, statement string, args []driver.NamedValue) (driver.Result, error) {
	c.observer.record(ctx, statement)
	return c.SQLiteConn.ExecContext(ctx, statement, args) //nolint:wrapcheck // Preserve database/sql driver sentinels and typed errors.
}
