package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
)

func TestCacheBuildJobsCoalesceAndRetryAfterFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	jobs := newCacheBuildJobs(t.Context(), nil, func(ctx context.Context, mode buildCacheMode) error {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return errors.New("synthetic build failure")
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	first, err := jobs.accept(buildCacheModeAuto)
	require.NoError(err)
	select {
	case <-started:
	case <-time.After(time.Second):
		require.FailNow("first cache job did not start")
	}
	second, err := jobs.accept(buildCacheModeAuto)
	require.NoError(err)
	assert.Equal(first.JobID, second.JobID)
	assert.Equal(int32(1), calls.Load())
	close(release)
	require.Eventually(func() bool {
		status, ok := jobs.status(first.JobID)
		return ok && status.Status == api.CacheBuildFailed
	}, time.Second, 10*time.Millisecond)
	retry, err := jobs.accept(buildCacheModeAuto)
	require.NoError(err)
	assert.NotEqual(first.JobID, retry.JobID)
	require.Eventually(func() bool {
		status, ok := jobs.status(retry.JobID)
		return ok && status.Status == api.CacheBuildFailed
	}, time.Second, 10*time.Millisecond)
	assert.Equal(int32(2), calls.Load())
}

func TestCacheBuildJobsVerifyWhenDue(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	started := make(chan buildCacheMode, 2)
	jobs := newCacheBuildJobs(t.Context(), nil, func(_ context.Context, mode buildCacheMode) error {
		started <- mode
		return nil
	})
	now := time.Now().UTC()
	interval := time.Hour
	require.NoError(jobs.verifyWhenDue(now, interval, now))
	assert.Empty(started)
	require.NoError(jobs.verifyWhenDue(now.Add(-interval), interval, now))
	require.Eventually(func() bool { return len(started) == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(buildCacheModeScheduledAuto, <-started)
	require.NoError(jobs.verifyWhenDue(now.Add(-interval), interval, now.Add(interval/2)))
	assert.Empty(started)
	require.NoError(jobs.verifyWhenDue(now.Add(-interval), interval, now.Add(interval)))
	require.Eventually(func() bool { return len(started) == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(buildCacheModeScheduledAuto, <-started)
}

func TestCacheBuildJobsSyncDuringBuildGetsFollowup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		release := make(chan struct{})
		var modes []buildCacheMode
		jobs := newCacheBuildJobs(t.Context(), nil, func(_ context.Context, mode buildCacheMode) error {
			modes = append(modes, mode)
			<-release
			return nil
		})
		first, err := jobs.accept(buildCacheModeAuto)
		require.NoError(err)
		synctest.Wait()
		pending, err := jobs.acceptAfterWrite(buildCacheModeScheduledAuto)
		require.NoError(err)
		assert.NotEqual(first.JobID, pending.JobID)
		assert.Equal(api.CacheBuildQueued, pending.Status)
		coalesced, err := jobs.acceptAfterWrite(buildCacheModeAuto)
		require.NoError(err)
		assert.Equal(pending.JobID, coalesced.JobID)
		release <- struct{}{}
		synctest.Wait()
		firstStatus, ok := jobs.status(first.JobID)
		require.True(ok)
		assert.Equal(api.CacheBuildPublished, firstStatus.Status)
		pendingStatus, ok := jobs.status(pending.JobID)
		require.True(ok)
		assert.Equal(api.CacheBuildRunning, pendingStatus.Status)
		assert.Equal([]buildCacheMode{buildCacheModeAuto, buildCacheModeAuto}, modes)
		// Once the follow-up has started, another write needs a later snapshot.
		next, err := jobs.acceptAfterWrite(buildCacheModeAuto)
		require.NoError(err)
		assert.NotEqual(pending.JobID, next.JobID)
		close(release)
		synctest.Wait()
		for _, id := range []string{pending.JobID, next.JobID} {
			status, ok := jobs.status(id)
			require.True(ok)
			assert.Equal(api.CacheBuildPublished, status.Status)
		}
	})
}

func TestCacheBuildJobsFailedVerificationRetriesSoon(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var calls atomic.Int32
	jobs := newCacheBuildJobs(t.Context(), nil, func(_ context.Context, mode buildCacheMode) error {
		calls.Add(1)
		return errors.New("synthetic verification failure")
	})
	now := time.Now().UTC()
	interval := time.Hour
	require.NoError(jobs.verifyWhenDue(now.Add(-interval), interval, now))
	require.Eventually(func() bool { return calls.Load() == 1 && !jobs.active() }, time.Second, 10*time.Millisecond)
	require.NoError(jobs.verifyWhenDue(now.Add(-interval), interval, now.Add(30*time.Second)))
	assert.Equal(int32(1), calls.Load())
	require.NoError(jobs.verifyWhenDue(now.Add(-interval), interval, now.Add(2*time.Minute)))
	require.Eventually(func() bool { return calls.Load() == 2 }, time.Second, 10*time.Millisecond)
}

func TestCacheBuildJobsWaitsForShutdown(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	jobs := newCacheBuildJobs(ctx, nil, func(context.Context, buildCacheMode) error {
		close(started)
		<-release
		return nil
	})
	_, err := jobs.accept(buildCacheModeAuto)
	require.NoError(err)
	<-started
	cancel()
	waitCtx, stopWait := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stopWait()
	require.False(jobs.waitContext(waitCtx))
	close(release)
	completeCtx, stopComplete := context.WithTimeout(context.Background(), time.Second)
	defer stopComplete()
	require.True(jobs.waitContext(completeCtx))
}

func TestManualSyncProbeDoesNotQueueCacheRefresh(t *testing.T) {
	assert := assert.New(t)
	assert.False(manualSyncCLICommand([]string{"sync-circleback", "--probe"}))
	assert.False(manualSyncCLICommand([]string{"sync-notion-meetings", "--probe=true"}))
	assert.True(manualSyncCLICommand([]string{"sync-notion-meetings", "--limit", "3"}))
}

func TestCacheBuildJobsScheduledCooldownCoversAllAccepts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var calls int
		jobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error {
			calls++
			return errors.New("synthetic build failure")
		})
		first, err := jobs.accept(buildCacheModeScheduledAuto)
		require.NoError(err)
		synctest.Wait()
		synctest.Sleep(30 * time.Second)
		for _, accept := range []func(buildCacheMode) (api.CacheBuildStatus, error){jobs.accept, jobs.acceptAfterWrite} {
			skipped, err := accept(buildCacheModeScheduledAuto)
			require.NoError(err)
			assert.Empty(skipped.JobID)
		}
		require.NoError(jobs.verifyWhenDue(time.Now().Add(-time.Hour), time.Hour, time.Now()))
		synctest.Wait()
		assert.Equal(1, calls)
		explicit, err := jobs.acceptAfterWrite(buildCacheModeAuto)
		require.NoError(err)
		assert.NotEmpty(explicit.JobID)
		synctest.Wait()
		assert.Equal(2, calls)
		synctest.Sleep(31 * time.Second)
		retry, err := jobs.accept(buildCacheModeScheduledAuto)
		require.NoError(err)
		assert.NotEmpty(retry.JobID)
		assert.NotEqual(first.JobID, retry.JobID)
		synctest.Wait()
		assert.Equal(3, calls)
	})
}

func TestCacheBuildJobsFailedBuildDefersOnlyScheduledFollowup(t *testing.T) {
	for _, mode := range []buildCacheMode{buildCacheModeScheduledAuto, buildCacheModeAuto} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				release := make(chan struct{})
				var calls int
				jobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error {
					calls++
					<-release
					if calls == 1 {
						return errors.New("synthetic build failure")
					}
					return nil
				})
				_, err := jobs.accept(buildCacheModeScheduledAuto)
				require.NoError(err)
				synctest.Wait()
				pending, err := jobs.acceptAfterWrite(mode)
				require.NoError(err)
				close(release)
				synctest.Wait()
				status, ok := jobs.status(pending.JobID)
				require.True(ok)
				if mode == buildCacheModeScheduledAuto {
					assert.Equal(1, calls)
					assert.Equal(api.CacheBuildFailed, status.Status)
					assert.Contains(status.Error, "retry")
				} else {
					assert.Equal(2, calls)
					assert.Equal(api.CacheBuildPublished, status.Status)
				}
			})
		})
	}
}

func TestCacheBuildJobsRetainsRecentCompletionsAndOutstandingJobs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		release := make(chan struct{})
		var block bool
		jobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error {
			if block {
				<-release
			}
			return nil
		})
		var completed []string
		for range 102 {
			job, err := jobs.accept(buildCacheModeAuto)
			require.NoError(err)
			completed = append(completed, job.JobID)
			synctest.Wait()
		}
		for i, id := range completed {
			_, ok := jobs.status(id)
			assert.Equal(i >= 2, ok)
		}
		block = true
		active, err := jobs.accept(buildCacheModeAuto)
		require.NoError(err)
		synctest.Wait()
		pending, err := jobs.acceptAfterWrite(buildCacheModeAuto)
		require.NoError(err)
		assert.NotEqual(active.JobID, pending.JobID)
		release <- struct{}{}
		synctest.Wait()
		_, ok := jobs.status(completed[2])
		assert.False(ok)
		status, ok := jobs.status(active.JobID)
		require.True(ok)
		assert.Equal(api.CacheBuildPublished, status.Status)
		status, ok = jobs.status(pending.JobID)
		require.True(ok)
		assert.Equal(api.CacheBuildRunning, status.Status)
		close(release)
		synctest.Wait()
	})
}

func TestCacheBuildJobsShutdownSettlesPendingAndQuietsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var logs bytes.Buffer
		oldLogger := logger
		logger = slog.New(slog.NewTextHandler(&logs, nil))
		t.Cleanup(func() { logger = oldLogger })
		var calls int
		jobs := newCacheBuildJobs(ctx, nil, func(ctx context.Context, _ buildCacheMode) error {
			calls++
			<-ctx.Done()
			return errors.New("subprocess terminated")
		})
		first, err := jobs.accept(buildCacheModeAuto)
		require.NoError(err)
		synctest.Wait()
		pending, err := jobs.acceptAfterWrite(buildCacheModeAuto)
		require.NoError(err)
		cancel()
		synctest.Wait()
		require.True(jobs.waitContext(t.Context()))
		assert.Equal(1, calls)
		for _, id := range []string{first.JobID, pending.JobID} {
			status, ok := jobs.status(id)
			require.True(ok)
			assert.Equal(api.CacheBuildFailed, status.Status)
			assert.Contains(status.Error, "shutting down")
			assert.NotNil(status.FinishedAt)
		}
		assert.Empty(logs.String())
		assert.False(jobs.active())
	})
}

func TestCacheBuildJobsLogsFailureWhileDaemonRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var logs bytes.Buffer
		oldLogger := logger
		logger = slog.New(slog.NewTextHandler(&logs, nil))
		t.Cleanup(func() { logger = oldLogger })
		jobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error {
			return errors.New("synthetic disk failure")
		})
		job, err := jobs.accept(buildCacheModeAuto)
		require.NoError(err)
		synctest.Wait()
		status, ok := jobs.status(job.JobID)
		require.True(ok)
		assert.Equal(api.CacheBuildFailed, status.Status)
		assert.Contains(logs.String(), "synthetic disk failure")
	})
}
