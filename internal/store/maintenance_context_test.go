package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type rollbackSignalConnector struct {
	rolledBack chan struct{}
	once       *sync.Once
	commit     func() error
}

func (c *rollbackSignalConnector) Connect(context.Context) (driver.Conn, error) {
	return &rollbackSignalConn{
		rolledBack: c.rolledBack,
		once:       c.once,
		commit:     c.commit,
	}, nil
}

func (c *rollbackSignalConnector) Driver() driver.Driver {
	return &rollbackSignalDriver{connector: c}
}

type rollbackSignalDriver struct {
	connector *rollbackSignalConnector
}

func (d *rollbackSignalDriver) Open(string) (driver.Conn, error) {
	return d.connector.Connect(context.Background())
}

type rollbackSignalConn struct {
	rolledBack chan struct{}
	once       *sync.Once
	commit     func() error
}

func (c *rollbackSignalConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (c *rollbackSignalConn) Close() error {
	return nil
}

func (c *rollbackSignalConn) Begin() (driver.Tx, error) {
	return &rollbackSignalTx{
		rolledBack: c.rolledBack,
		once:       c.once,
		commit:     c.commit,
	}, nil
}

func (c *rollbackSignalConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}

type rollbackSignalTx struct {
	rolledBack chan struct{}
	once       *sync.Once
	commit     func() error
}

func (tx *rollbackSignalTx) Commit() error {
	if tx.commit != nil {
		return tx.commit()
	}
	return nil
}

func (tx *rollbackSignalTx) Rollback() error {
	tx.once.Do(func() {
		close(tx.rolledBack)
	})
	return nil
}

func TestRunMaintenancePreservesCanceledContextAfterAsyncRollback(t *testing.T) {
	rolledBack := make(chan struct{})
	connector := &rollbackSignalConnector{
		rolledBack: rolledBack,
		once:       &sync.Once{},
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	st := &Store{
		db:      newLoggedDB(db, identityRebind),
		dialect: &SQLiteDialect{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	err := st.runMaintenance(ctx, func(context.Context, *loggedTx) error {
		cancel()
		select {
		case <-rolledBack:
			return nil
		case <-time.After(time.Second):
			require.FailNow(t, "database/sql did not roll back the canceled transaction")
			return nil
		}
	})

	require.ErrorIs(t, err, context.Canceled)
}

func TestRunMaintenancePreservesCommitErrorWhenContextIsCanceledDuringCommit(t *testing.T) {
	commitErr := errors.New("commit failed")
	ctx, cancel := context.WithCancel(context.Background())
	connector := &rollbackSignalConnector{
		rolledBack: make(chan struct{}),
		once:       &sync.Once{},
		commit: func() error {
			cancel()
			return commitErr
		},
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	st := &Store{
		db:      newLoggedDB(db, identityRebind),
		dialect: &SQLiteDialect{},
	}

	err := st.runMaintenance(ctx, func(context.Context, *loggedTx) error {
		return nil
	})

	require.ErrorIs(t, err, commitErr)
}
