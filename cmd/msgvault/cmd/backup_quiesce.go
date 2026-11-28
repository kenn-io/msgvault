package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/api"
)

// cacheBuilderLockPollInterval is how often backup quiesce retries the
// analytics builder lock while another builder holds it.
const cacheBuilderLockPollInterval = 50 * time.Millisecond

// daemonBackupQuiesce stops the daemon's analytics cache work for a backup
// freeze. Cache refreshes and build jobs run outside the operation gate and
// hold a SQLite read transaction for their whole export, which keeps a
// truncating WAL checkpoint busy. The returned function cancels and holds the
// daemon's own refreshes and builds, then takes the cross-process builder lock
// so a builder in another msgvault process, or one the daemon started outside
// these managers, cannot keep reading either. Interrupted work runs again
// after release.
func daemonBackupQuiesce(
	refresher *backgroundCacheRefresher, jobs *cacheBuildJobs, analyticsDir string,
) api.BackupQuiesceFunc {
	return func(ctx context.Context) (func(), error) {
		resumeRefresher := func() {}
		if refresher != nil {
			resume, err := refresher.pause(ctx)
			if err != nil {
				return nil, fmt.Errorf("the background analytics cache refresh did not stop: %w", err)
			}
			resumeRefresher = resume
		}
		resumeJobs, err := jobs.pause(ctx)
		if err != nil {
			resumeRefresher()
			return nil, fmt.Errorf("the running analytics cache build did not stop: %w", err)
		}
		resumeAll := func() {
			resumeJobs()
			resumeRefresher()
		}

		lock, err := cacheBuilderFileLock(analyticsDir)
		if err != nil {
			resumeAll()
			return nil, err
		}
		locked, err := lock.TryLockContext(ctx, cacheBuilderLockPollInterval)
		if err != nil || !locked {
			resumeAll()
			if err == nil {
				err = errors.New("lock not acquired")
			}
			return nil, fmt.Errorf("another analytics cache build is still reading the archive (lock %s): %w",
				lock.Path(), err)
		}
		return func() {
			_ = lock.Unlock()
			resumeAll()
		}, nil
	}
}
