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
// removal delete the directory wholesale, and unlinking a held lock file
// would let another process acquire a fresh one and write concurrently.
func CacheBuildLockPath(analyticsDir string) string {
	return filepath.Clean(analyticsDir) + ".build.lock"
}

// cacheReadLockRetryInterval bounds how often a blocked reader re-attempts
// the shared cache lock while a build holds it exclusively.
const cacheReadLockRetryInterval = 50 * time.Millisecond

// acquireCacheReadLock takes the shared cache lock for the duration of one
// query, blocking while a cache build holds it exclusively (builds take
// seconds). Shared holders do not conflict with each other, so concurrent
// queries and nested engine calls proceed freely. The returned release
// function must be deferred by the caller.
func acquireCacheReadLock(ctx context.Context, analyticsDir string) (func(), error) {
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
