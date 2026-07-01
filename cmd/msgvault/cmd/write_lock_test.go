package cmd

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteOwnerLockPath(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")

	assert.Equal(t,
		filepath.Join(dataDir, "db.write.lock"),
		writeOwnerLockPath(dataDir),
		"write owner lock path")
}

func TestTryAcquireWriteOwnerLockExcludesSecondOwner(t *testing.T) {
	dataDir := t.TempDir()

	first, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(t, err, "first lock")
	t.Cleanup(func() { require.NoError(t, first.Close(), "close first lock") })

	second, err := tryAcquireWriteOwnerLock(dataDir)
	assert.Nil(t, second, "second lock")
	require.Error(t, err, "second acquisition should fail")
	require.ErrorAs(t, err, &writeOwnerLockHeldError{}, "error type")
	assert.Contains(t, err.Error(), "msgvault process", "error text")

	require.NoError(t, first.Close(), "release first lock")

	third, err := acquireWriteOwnerLock(context.Background(), dataDir)
	require.NoError(t, err, "third lock after release")
	require.NoError(t, third.Close(), "close third lock")
}
