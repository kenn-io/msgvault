package cacheops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

// TestCollectStatsWaitsForCacheBuildLock pins that stats collection
// participates in the cache reader/writer protocol: a concurrent build's
// exclusive lock must block collection so files cannot vanish mid-scan.
func TestCollectStatsWaitsForCacheBuildLock(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dir, tableMessages), 0o755), "create messages dir")

	build := flock.New(query.CacheBuildLockPath(dir))
	locked, err := build.TryLock()
	require.NoError(err, "acquire exclusive build lock")
	require.True(locked, "acquire exclusive build lock")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = CollectStats(ctx, dir)
	require.Error(err, "stats collection must wait while a build holds the lock")

	require.NoError(build.Unlock(), "release build lock")
	stats, err := CollectStats(context.Background(), dir)
	require.NoError(err, "stats collection after the build releases")
	require.Equal(StatusNoCacheData, stats.Status, "empty messages dir has no cache data")
}
