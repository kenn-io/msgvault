package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The busy-snapshot race this retry exists for cannot be provoked
// deterministically through the public API, so the retry contract is proven
// directly: transient busy retries, other errors pass straight through,
// persistent busy gives up at the attempt cap, and a cancelled context ends
// the wait instead of sleeping through the budget.
func TestRetryContendedWriteErrRetriesTransientBusyOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, err := Open(filepath.Join(t.TempDir(), "retry.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = s.Close() })
	busy := sqlite3.Error{Code: sqlite3.ErrBusy}

	attempts := 0
	require.NoError(retryContendedWriteErr(context.Background(), s, "probe", func() error {
		attempts++
		if attempts < 3 {
			return busy
		}
		return nil
	}))
	assert.Equal(3, attempts)

	permanent := errors.New("not busy")
	attempts = 0
	err = retryContendedWriteErr(context.Background(), s, "probe", func() error {
		attempts++
		return permanent
	})
	require.ErrorIs(err, permanent)
	assert.Equal(1, attempts)

	attempts = 0
	err = retryContendedWriteErr(context.Background(), s, "probe", func() error {
		attempts++
		return busy
	})
	require.ErrorIs(err, busy)
	assert.Equal(maxContendedWriteAttempts, attempts,
		"persistent busy must stop at the attempt cap")
}

func TestRetryContendedWriteStopsWaitingWhenContextIsCancelled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, err := Open(filepath.Join(t.TempDir(), "retry.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = s.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	started := time.Now()
	err = retryContendedWriteErr(ctx, s, "probe", func() error {
		attempts++
		cancel()
		return sqlite3.Error{Code: sqlite3.ErrBusy}
	})
	require.ErrorIs(err, context.Canceled)
	assert.Equal(1, attempts, "a cancelled context must not be retried")
	assert.Less(time.Since(started), contendedWriteBackoffMax,
		"cancellation must cut the backoff short")
}
