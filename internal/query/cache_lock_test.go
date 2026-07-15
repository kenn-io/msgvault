package query

import (
	"context"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
)

// TestAcquireCacheReadLockBlocksDuringBuild pins the reader/writer protocol:
// a query's shared hold must wait out a build's exclusive hold instead of
// reading Parquet files mid-mutation, and must acquire promptly once the
// build releases.
func TestAcquireCacheReadLockBlocksDuringBuild(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()

	build := flock.New(CacheBuildLockPath(dir))
	locked, err := build.TryLock()
	require.NoError(err, "acquire exclusive build lock")
	require.True(locked, "acquire exclusive build lock")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = acquireCacheReadLock(ctx, dir)
	require.Error(err, "reader must block while a build holds the lock exclusively")

	require.NoError(build.Unlock(), "release build lock")
	release, err := acquireCacheReadLock(context.Background(), dir)
	require.NoError(err, "reader must acquire after the build releases")
	release()
}

// TestAcquireCacheReadLockSharedHoldersDoNotConflict pins that concurrent
// queries never block each other: only builds hold the lock exclusively.
func TestAcquireCacheReadLockSharedHoldersDoNotConflict(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()

	first, err := acquireCacheReadLock(context.Background(), dir)
	require.NoError(err, "first shared holder")
	defer first()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	second, err := acquireCacheReadLock(ctx, dir)
	require.NoError(err, "second shared holder must not conflict with the first")
	second()
}
