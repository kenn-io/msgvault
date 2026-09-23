package cmd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
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
	require := require.New(t)
	assert := assert.New(t)
	started := make(chan buildCacheMode, 2)
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	jobs := newCacheBuildJobs(t.Context(), nil, func(_ context.Context, mode buildCacheMode) error {
		if calls.Add(1) == 1 {
			started <- mode
			<-releaseFirst
			return nil
		}
		started <- mode
		return nil
	})
	first, err := jobs.accept(buildCacheModeAuto)
	require.NoError(err)
	require.Equal(buildCacheModeAuto, <-started)
	coalesced, err := jobs.acceptAfterWrite(buildCacheModeScheduledAuto)
	require.NoError(err)
	assert.Equal(first.JobID, coalesced.JobID)
	close(releaseFirst)
	require.Eventually(func() bool { return calls.Load() == 2 }, time.Second, 10*time.Millisecond)
	assert.Equal(buildCacheModeScheduledAuto, <-started)
	require.Eventually(func() bool { return !jobs.active() }, time.Second, 10*time.Millisecond)
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
