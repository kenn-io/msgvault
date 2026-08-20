package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// postgreSQLRowLock names one row a blocker transaction locks FOR UPDATE.
type postgreSQLRowLock struct {
	table string
	id    int64
}

// forcePostgreSQLDeadlock closes a lock cycle around write. A blocker
// transaction holds the held row; write is started and parked behind it; then
// the blocker asks for the cycle row, which write is expected to hold by then,
// so PostgreSQL's detector has to abort one side. The write is parked well
// before the blocker closes the cycle so that its deadlock_timeout expires
// first and it, not the blocker, is the victim. The blocker must release
// cleanly; the write's own outcome is returned for the caller to judge.
func forcePostgreSQLDeadlock(
	ctx context.Context, t *testing.T, st *store.Store,
	held, cycle postgreSQLRowLock, write func(context.Context) error,
) error {
	t.Helper()
	require := require.New(t)
	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	var lockedID int64
	require.NoError(blocker.QueryRowContext(ctx,
		`SELECT id FROM `+held.table+` WHERE id = $1 FOR UPDATE`, held.id).Scan(&lockedID))
	require.Equal(held.id, lockedID)

	writeDone := make(chan error, 1)
	go func() { writeDone <- write(ctx) }()
	require.Eventually(func() bool {
		return postgreSQLWaitingLockCount(t, st) >= 1
	}, 5*time.Second, 10*time.Millisecond, "write did not reach the held row lock")
	time.Sleep(250 * time.Millisecond)

	blockerDone := make(chan error, 1)
	go func() {
		var id int64
		lockErr := blocker.QueryRowContext(ctx,
			`SELECT id FROM `+cycle.table+` WHERE id = $1 FOR UPDATE`, cycle.id).Scan(&id)
		if lockErr == nil {
			lockErr = blocker.Commit()
		} else {
			_ = blocker.Rollback()
		}
		blockerDone <- lockErr
	}()

	var writeErr, blockerErr error
	select {
	case writeErr = <-writeDone:
	case <-ctx.Done():
		require.FailNow("write did not finish after the deadlock detector", ctx.Err())
	}
	select {
	case blockerErr = <-blockerDone:
	case <-ctx.Done():
		require.FailNow("blocker did not finish after the deadlock detector", ctx.Err())
	}
	require.NoError(blockerErr, "blocker must release its row lock")
	return writeErr
}
