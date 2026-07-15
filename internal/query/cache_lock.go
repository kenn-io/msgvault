package query

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// CacheBuildLockPath returns the cross-process lock file that coordinates
// Parquet cache access: builders hold it exclusively while mutating cache
// files, and DuckDB readers hold it shared per query so a rebuild cannot
// delete files out from under a running query. The lock lives next to the
// analytics directory rather than inside it because builds and account
// recovery replace live cache paths, and unlinking a held lock file would let
// another process acquire a fresh one and write concurrently.
func CacheBuildLockPath(analyticsDir string) string {
	return filepath.Clean(analyticsDir) + ".build.lock"
}

// cacheReadLockRetryInterval bounds how often a blocked reader re-attempts
// the shared cache lock while a build holds it exclusively.
const cacheReadLockRetryInterval = 50 * time.Millisecond

// AcquireCacheReadLock takes the shared cache lock for the duration of one
// read, blocking while a cache build holds it exclusively (builds take
// seconds). Shared holders do not conflict with each other, so concurrent
// queries and nested engine calls proceed freely. The returned release
// function must be deferred by the caller. Every direct Parquet reader —
// the DuckDB engine and cacheops alike — must hold this lock so builds
// cannot remove files mid-read.
func AcquireCacheReadLock(ctx context.Context, analyticsDir string) (func(), error) {
	lock := flock.New(CacheBuildLockPath(analyticsDir))
	locked, err := lock.TryRLockContext(ctx, cacheReadLockRetryInterval)
	if err != nil {
		return nil, fmt.Errorf("acquire shared analytics cache lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("acquire shared analytics cache lock %s: not acquired", lock.Path())
	}
	return func() { _ = lock.Unlock() }, nil
}

// AcquireReadyCacheReadLock takes the shared cache lock and validates the
// commit marker before any Parquet path is touched. Callers must release the
// returned lock after their read completes.
func AcquireReadyCacheReadLock(ctx context.Context, analyticsDir string) (func(), error) {
	release, err := AcquireCacheReadLock(ctx, analyticsDir)
	if err != nil {
		return nil, err
	}
	readiness, err := InspectCacheReadiness(analyticsDir)
	if err != nil {
		release()
		return nil, err
	}
	if readiness != CacheReady {
		release()
		return nil, fmt.Errorf("%w: cache is %s", ErrCacheUnavailable, readiness)
	}
	return release, nil
}
