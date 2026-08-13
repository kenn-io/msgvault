package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The busy-snapshot race this retry exists for cannot be provoked
// deterministically through the public API, so the retry contract is proven
// directly: transient busy retries, other errors pass straight through, and
// persistent busy gives up at the attempt cap.
func TestRetryRelatedOnBusyRetriesTransientBusyOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, err := Open(filepath.Join(t.TempDir(), "retry.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = s.Close() })
	busy := sqlite3.Error{Code: sqlite3.ErrBusy}

	attempts := 0
	require.NoError(s.retryRelatedOnBusy(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return busy
		}
		return nil
	}))
	assert.Equal(3, attempts)

	permanent := errors.New("not busy")
	attempts = 0
	err = s.retryRelatedOnBusy(context.Background(), func() error {
		attempts++
		return permanent
	})
	require.ErrorIs(err, permanent)
	assert.Equal(1, attempts)

	attempts = 0
	err = s.retryRelatedOnBusy(context.Background(), func() error {
		attempts++
		return busy
	})
	require.ErrorIs(err, busy)
	assert.Equal(5, attempts, "persistent busy must stop at the attempt cap")
}
