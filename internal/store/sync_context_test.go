package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type canceledStartSyncConnector struct {
	cancel      context.CancelFunc
	rollbackCtx chan error
}

func (c *canceledStartSyncConnector) Connect(context.Context) (driver.Conn, error) {
	return &canceledStartSyncConn{cancel: c.cancel, rollbackCtx: c.rollbackCtx}, nil
}

func (c *canceledStartSyncConnector) Driver() driver.Driver {
	return &canceledStartSyncDriver{connector: c}
}

type canceledStartSyncDriver struct {
	connector *canceledStartSyncConnector
}

func (d *canceledStartSyncDriver) Open(string) (driver.Conn, error) {
	return d.connector.Connect(context.Background())
}

type canceledStartSyncConn struct {
	cancel      context.CancelFunc
	rollbackCtx chan error
}

func (c *canceledStartSyncConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (c *canceledStartSyncConn) Close() error { return nil }

func (c *canceledStartSyncConn) Begin() (driver.Tx, error) {
	return nil, driver.ErrSkip
}

func (c *canceledStartSyncConn) ExecContext(
	ctx context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Result, error) {
	if strings.TrimSpace(query) == "ROLLBACK" {
		c.rollbackCtx <- ctx.Err()
	}
	return driver.RowsAffected(1), nil
}

func (c *canceledStartSyncConn) QueryContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	if strings.Contains(query, "INSERT INTO sync_runs") {
		c.cancel()
		return nil, context.Canceled
	}
	if strings.Contains(query, "FROM sync_runs") {
		return &singleInt64Rows{read: true}, nil
	}
	if strings.Contains(query, "FROM sync_operations") {
		return &singleInt64Rows{read: true}, nil
	}
	return &singleInt64Rows{value: 1}, nil
}

type singleInt64Rows struct {
	value int64
	read  bool
}

func (r *singleInt64Rows) Columns() []string { return []string{"id"} }
func (r *singleInt64Rows) Close() error      { return nil }
func (r *singleInt64Rows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	dest[0] = r.value
	return nil
}

func TestStartSyncContextRollbackOutlivesRequestCancellation(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rollbackCtx := make(chan error, 1)
	db := sql.OpenDB(&canceledStartSyncConnector{
		cancel:      cancel,
		rollbackCtx: rollbackCtx,
	})
	t.Cleanup(func() {
		require.NoError(db.Close())
	})
	st := &Store{
		db:      newLoggedDB(db, identityRebind),
		dialect: &SQLiteDialect{},
	}

	_, err := st.StartSyncContext(ctx, 1, "gmail")
	require.ErrorIs(err, context.Canceled)
	select {
	case rollbackErr := <-rollbackCtx:
		require.NoError(rollbackErr, "rollback must use a context independent of request cancellation")
	case <-time.After(time.Second):
		require.FailNow("manual transaction rollback was not attempted")
	}
}
