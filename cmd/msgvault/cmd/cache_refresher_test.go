package cmd

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefresherCoalescesRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var runs atomic.Int32
		release := make(chan struct{})
		var mu sync.Mutex
		var seen []string
		r := newBackgroundCacheRefresher(context.Background(), func(ctx context.Context, id string) error {
			mu.Lock()
			seen = append(seen, id)
			mu.Unlock()
			if runs.Add(1) == 1 {
				<-release
			}
			return nil
		}, nil)

		require.True(r.Request("first"))
		synctest.Wait()
		require.True(r.Request("second"))
		require.True(r.Request("third"))
		require.True(r.Request("fourth"))
		close(release)
		synctest.Wait()

		assert.Equal(int32(2), runs.Load(), "requests during a run collapse into one follow-up")
		assert.Equal([]string{"first", "fourth"}, seen)
		require.NoError(r.Shutdown(context.Background()))
	})
}

func TestRefresherShutdownCancelsAndWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		started := make(chan struct{})
		var finished atomic.Bool
		r := newBackgroundCacheRefresher(context.Background(), func(ctx context.Context, id string) error {
			close(started)
			<-ctx.Done()
			time.Sleep(time.Second)
			finished.Store(true)
			return ctx.Err()
		}, nil)

		require.True(r.Request("sync"))
		<-started
		require.NoError(r.Shutdown(context.Background()))
		assert.True(finished.Load(), "Shutdown returns only after the running build stops")
		assert.False(r.Request("late"), "requests after shutdown are refused")
	})
}

func TestRefresherShutdownStopsDelayedRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var runs atomic.Int32
		r := newBackgroundCacheRefresher(context.Background(), func(context.Context, string) error {
			runs.Add(1)
			return nil
		}, nil)
		r.RequestAfter(time.Hour, "startup")
		time.Sleep(30 * time.Minute)
		require.NoError(r.Shutdown(context.Background()))
		time.Sleep(time.Hour)
		synctest.Wait()
		assert.Equal(int32(0), runs.Load())

		r2 := newBackgroundCacheRefresher(context.Background(), func(context.Context, string) error {
			runs.Add(1)
			return nil
		}, nil)
		r2.RequestAfter(time.Hour, "startup")
		time.Sleep(time.Hour + time.Second)
		synctest.Wait()
		assert.Equal(int32(1), runs.Load(), "delayed request fires after its delay")
		require.NoError(r2.Shutdown(context.Background()))
	})
}

func TestShutdownBackgroundCacheRefresherStopsDelayedRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var runs atomic.Int32
		r := newBackgroundCacheRefresher(context.Background(), func(context.Context, string) error {
			runs.Add(1)
			return nil
		}, nil)
		r.RequestAfter(time.Hour, "startup")

		require.NoError(shutdownBackgroundCacheRefresher(r))
		time.Sleep(time.Hour)
		synctest.Wait()
		assert.Zero(runs.Load(), "early startup cleanup cancels the delayed rebuild")
	})
}

func TestBuildCacheSubprocessWaitHonorsCancellation(t *testing.T) {
	require := require.New(t)
	buildCacheMu.Lock()
	locked := true
	unlock := func() {
		if locked {
			buildCacheMu.Unlock()
			locked = false
		}
	}
	t.Cleanup(unlock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		done <- buildCacheSubprocessMode(ctx, buildCacheModeAuto)
	}()

	select {
	case err := <-done:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(250 * time.Millisecond):
		unlock()
		<-done
		require.FailNow("canceled cache build must stop waiting for the in-process build lock")
	}
	unlock()
}

func TestRefresherRequestAfterKeepsOnlyEarliestTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var runs atomic.Int32
		r := newBackgroundCacheRefresher(context.Background(), func(context.Context, string) error {
			runs.Add(1)
			return nil
		}, nil)

		r.RequestAfter(2*time.Hour, "later")
		r.RequestAfter(time.Hour, "sooner")
		r.RequestAfter(3*time.Hour, "latest")
		time.Sleep(time.Hour + time.Second)
		synctest.Wait()
		assert.Equal(int32(1), runs.Load(), "the earliest delayed request fires")

		time.Sleep(3 * time.Hour)
		synctest.Wait()
		assert.Equal(int32(1), runs.Load(), "later delayed requests were folded into the earliest")
		require.NoError(r.Shutdown(context.Background()))
	})
}
