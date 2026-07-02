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
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()

	first, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "first lock")

	t.Cleanup(func() { require.NoError(first.Close(), "close first lock") })

	second, err := tryAcquireWriteOwnerLock(dataDir)
	assert.Nil(second, "second lock")
	require.Error(err, "second acquisition should fail")
	require.ErrorAs(err, &writeOwnerLockHeldError{}, "error type")
	assert.Contains(err.Error(), "msgvault process", "error text")
	require.NoError(
		first.Close(), "release first lock")

	third, err := acquireWriteOwnerLock(context.Background(), dataDir)
	require.NoError(
		err, "third lock after release")

	require.NoError(
		third.Close(), "close third lock")
}
