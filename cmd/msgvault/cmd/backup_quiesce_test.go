package cmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/api"
)

// quiesceFreezeCoordinator plays the daemon's side of a backup freeze for
// Kit: Begin runs the quiesce hook the freeze handler runs, End releases it.
type quiesceFreezeCoordinator struct {
	quiesce api.BackupQuiesceFunc
	release func()
}

func (c *quiesceFreezeCoordinator) Begin(ctx context.Context) error {
	release, err := c.quiesce(ctx)
	if err != nil {
		return err
	}
	c.release = release
	return nil
}

func (c *quiesceFreezeCoordinator) End(context.Context) error {
	c.release()
	return nil
}

// newWALArchive creates a synthetic WAL database whose WAL holds frames, and
// returns it with its writer connection still open so nothing checkpoints it.
func newWALArchive(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
	writer, err := sql.Open("sqlite3", "file:"+dbPath+"?_journal_mode=WAL")
	require.NoError(t, err)
	writer.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = writer.Close() })
	ctx := t.Context()
	_, err = writer.ExecContext(ctx, "PRAGMA wal_autocheckpoint=0")
	require.NoError(t, err)
	_, err = writer.ExecContext(ctx, "CREATE TABLE messages (id INTEGER PRIMARY KEY, subject TEXT)")
	require.NoError(t, err)
	_, err = writer.ExecContext(ctx, "INSERT INTO messages (subject) VALUES ('synthetic one'), ('synthetic two')")
	require.NoError(t, err)
	return dbPath
}

// holdReadTransaction opens a read transaction on dbPath, as a cache build's
// source snapshot does, and keeps it until ctx ends.
func holdReadTransaction(ctx context.Context, dbPath string, started chan<- struct{}) error {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(context.WithoutCancel(ctx), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(context.WithoutCancel(ctx), "SELECT count(*) FROM messages").Scan(&n); err != nil {
		return err
	}
	started <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func checkpointBusy(t *testing.T, dbPath string) bool {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=0")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var busy, logFrames, checkpointed int
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)").
		Scan(&busy, &logFrames, &checkpointed))
	return busy != 0
}

func TestBackupQuiesceStopsCacheRefreshHoldingReadTransaction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := newWALArchive(t)

	started := make(chan struct{}, 4)
	var runs atomic.Int32
	refresher := newBackgroundCacheRefresher(t.Context(), func(ctx context.Context, _ string) error {
		runs.Add(1)
		return holdReadTransaction(ctx, dbPath, started)
	}, nil)
	t.Cleanup(func() { _ = refresher.Shutdown(context.WithoutCancel(t.Context())) })

	require.True(refresher.Request("sync"))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow("cache refresh did not open its read transaction")
	}
	require.True(checkpointBusy(t, dbPath), "the held read transaction must block a truncating checkpoint")

	jobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error { return nil })
	coordinator := &quiesceFreezeCoordinator{
		quiesce: daemonBackupQuiesce(refresher, jobs, filepath.Join(t.TempDir(), "analytics")),
	}
	session, err := backup.OpenFrozenSession(t.Context(), dbPath, coordinator)
	require.NoError(err, "freeze must checkpoint once the daemon's reader stops")
	t.Cleanup(func() { _ = session.Close() })
	assert.Positive(session.PageCount)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow("interrupted cache refresh did not run again after the freeze")
	}
	assert.Equal(int32(2), runs.Load())
}

func TestBackupQuiesceReportsBuilderLockHolderAndResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		analyticsDir := filepath.Join(t.TempDir(), "analytics")
		other, err := cacheBuilderFileLock(analyticsDir)
		require.NoError(err)
		held, err := other.TryLock()
		require.NoError(err)
		require.True(held)
		t.Cleanup(func() { _ = other.Unlock() })

		ran := make(chan struct{}, 1)
		refresher := newBackgroundCacheRefresher(t.Context(), func(context.Context, string) error {
			ran <- struct{}{}
			return nil
		}, nil)
		t.Cleanup(func() { _ = refresher.Shutdown(context.WithoutCancel(t.Context())) })
		jobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error { return nil })
		quiesce := daemonBackupQuiesce(refresher, jobs, analyticsDir)

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		release, err := quiesce(ctx)
		require.Error(err)
		assert.Nil(release)
		assert.Contains(err.Error(), "another analytics cache build is still reading the archive")
		assert.Contains(err.Error(), other.Path())

		// A failed quiesce leaves nothing paused.
		require.True(refresher.Request("sync"))
		select {
		case <-ran:
		case <-time.After(5 * time.Second):
			require.FailNow("refresher stayed paused after a failed quiesce")
		}
		require.NoError(other.Unlock())
		assert.True(builderLockFree(t, analyticsDir), "a failed quiesce must not keep the builder lock")
	})
}

// builderLockFree reports whether the builder lock is free by taking and
// dropping it.
func builderLockFree(t *testing.T, analyticsDir string) bool {
	t.Helper()
	probe, err := cacheBuilderFileLock(analyticsDir)
	require.NoError(t, err)
	got, err := probe.TryLock()
	require.NoError(t, err)
	if got {
		require.NoError(t, probe.Unlock())
	}
	return got
}

func TestCacheBuildJobsPauseRequeuesInterruptedBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	started := make(chan struct{}, 2)
	var calls atomic.Int32
	jobs := newCacheBuildJobs(t.Context(), nil, func(ctx context.Context, _ buildCacheMode) error {
		if calls.Add(1) == 1 {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		started <- struct{}{}
		return nil
	})
	job, err := jobs.accept(buildCacheModeAuto)
	require.NoError(err)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow("cache build did not start")
	}

	resume, err := jobs.pause(t.Context())
	require.NoError(err)
	status, ok := jobs.status(job.JobID)
	require.True(ok)
	assert.Equal(api.CacheBuildQueued, status.Status, "interrupted build keeps its job queued")

	// Requests during the pause join the queued job instead of starting.
	again, err := jobs.accept(buildCacheModeAuto)
	require.NoError(err)
	assert.Equal(job.JobID, again.JobID)
	assert.Equal(int32(1), calls.Load())

	resume()
	require.Eventually(func() bool {
		status, ok := jobs.status(job.JobID)
		return ok && status.Status == api.CacheBuildPublished
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(int32(2), calls.Load())
}

func TestCacheBuildJobsRunInterruptedBuildAfterPauseGivesUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		started := make(chan struct{}, 2)
		stop := make(chan struct{})
		var calls atomic.Int32
		jobs := newCacheBuildJobs(t.Context(), nil, func(ctx context.Context, _ buildCacheMode) error {
			if calls.Add(1) == 1 {
				started <- struct{}{}
				<-ctx.Done()
				<-stop // the build is slow to stop
				return ctx.Err()
			}
			return nil
		})
		job, err := jobs.accept(buildCacheModeAuto)
		require.NoError(err)
		<-started

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err = jobs.pause(ctx)
		require.ErrorIs(err, context.DeadlineExceeded)

		close(stop)
		synctest.Wait()
		status, ok := jobs.status(job.JobID)
		require.True(ok)
		assert.Equal(api.CacheBuildPublished, status.Status, "the interrupted job runs without another resume")
		assert.Equal(int32(2), calls.Load())
	})
}
