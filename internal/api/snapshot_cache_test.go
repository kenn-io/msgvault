package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

func TestSnapshotCacheFastComputeIsFresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		var cache snapshotCache[int]
		value, asOf, stale, err := cache.get(context.Background(), context.Background(), "k", 2*time.Second,
			func(context.Context) (int, error) { return 7, nil })
		require.NoError(t, err)
		assert.Equal(7, value)
		assert.False(stale)
		assert.False(asOf.IsZero())
	})
}

func TestSnapshotCacheKeepsRequestIDWithServerLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var cache snapshotCache[int]
		reqCtx, cancelRequest := context.WithCancel(store.WithRequestID(context.Background(), "stats-request"))
		defer cancelRequest()
		serverCtx, cancelServer := context.WithCancel(context.Background())
		defer cancelServer()
		started := make(chan context.Context, 1)
		finished := make(chan error, 1)
		returned := make(chan error, 1)
		go func() {
			_, _, _, err := cache.get(reqCtx, serverCtx, "stats", time.Second, func(ctx context.Context) (int, error) {
				started <- ctx
				<-ctx.Done()
				finished <- ctx.Err()
				return 0, ctx.Err()
			})
			returned <- err
		}()
		computeCtx := <-started
		assert.Equal("stats-request", store.RequestIDFromContext(computeCtx))
		cancelRequest()
		require.ErrorIs(<-returned, context.Canceled)
		synctest.Wait()
		require.NoError(computeCtx.Err(), "a disconnected request must not cancel a shared refresh")
		cancelServer()
		require.ErrorIs(<-finished, context.Canceled)
	})
}

func TestSnapshotCacheSlowComputeServesPreviousValue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var cache snapshotCache[int]
		_, _, _, err := cache.get(context.Background(), context.Background(), "k", 2*time.Second,
			func(context.Context) (int, error) { return 1, nil })
		require.NoError(err)
		firstAsOf := time.Now()

		time.Sleep(time.Minute)
		value, asOf, stale, err := cache.get(context.Background(), context.Background(), "k", 2*time.Second,
			func(context.Context) (int, error) {
				time.Sleep(10 * time.Second)
				return 2, nil
			})
		require.NoError(err)
		assert.Equal(1, value, "a slow refresh serves the previous snapshot")
		assert.True(stale)
		assert.Equal(firstAsOf, asOf)

		time.Sleep(10 * time.Second)
		synctest.Wait()
		value, _, stale, err = cache.get(context.Background(), context.Background(), "k", 2*time.Second,
			func(context.Context) (int, error) { return 3, nil })
		require.NoError(err)
		assert.Equal(3, value, "a fast later compute is fresh again")
		assert.False(stale)
	})
}

func TestSnapshotCacheCanceledRefreshWaitDoesNotServeStaleValue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var cache snapshotCache[int]
		_, _, _, err := cache.get(context.Background(), context.Background(), "k", time.Second,
			func(context.Context) (int, error) { return 1, nil })
		require.NoError(err)

		reqCtx, cancelRequest := context.WithCancel(context.Background())
		defer cancelRequest()
		started := make(chan struct{})
		release := make(chan struct{})
		defer func() {
			close(release)
			synctest.Wait()
		}()
		type result struct {
			value int
			asOf  time.Time
			stale bool
			err   error
		}
		returned := make(chan result, 1)
		go func() {
			value, asOf, stale, err := cache.get(reqCtx, context.Background(), "k", time.Second,
				func(context.Context) (int, error) {
					close(started)
					<-release
					return 2, nil
				})
			returned <- result{value: value, asOf: asOf, stale: stale, err: err}
		}()
		<-started
		cancelRequest()
		got := <-returned
		require.ErrorIs(got.err, context.Canceled)
		assert.Zero(got.value)
		assert.True(got.asOf.IsZero())
		assert.False(got.stale)
	})
}

func TestSnapshotCacheWithoutPreviousValueTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var cache snapshotCache[int]
		started := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseCompute := func() { releaseOnce.Do(func() { close(release) }) }
		defer func() {
			releaseCompute()
			synctest.Wait()
		}()
		type result struct {
			value int
			stale bool
			err   error
		}
		returned := make(chan result, 1)
		go func() {
			value, _, stale, err := cache.get(t.Context(), t.Context(), "k", 2*time.Second,
				func(context.Context) (int, error) {
					close(started)
					<-release
					return 5, nil
				})
			returned <- result{value: value, stale: stale, err: err}
		}()
		<-started
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case got := <-returned:
			require.ErrorIs(got.err, context.DeadlineExceeded, "the first request waits only for its budget")
			assert.Zero(got.value, "there is no stale value to serve before the first snapshot")
			assert.False(got.stale)
		default:
			releaseCompute()
			got := <-returned
			require.ErrorIs(got.err, context.DeadlineExceeded, "the first request waits only for its budget")
		}
	})
}

func TestSnapshotCacheSharesInFlightCompute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cache snapshotCache[int]
		var computes atomic.Int32
		var computeErr error
		compute := func(context.Context) (int, error) {
			computes.Add(1)
			time.Sleep(time.Second)
			return 4, computeErr
		}
		done := make(chan int, 2)
		for range 2 {
			go func() {
				value, _, _, _ := cache.get(context.Background(), context.Background(), "k", 2*time.Second, compute)
				done <- value
			}()
		}
		assert.Equal(t, 4, <-done)
		assert.Equal(t, 4, <-done)
		assert.Equal(t, int32(1), computes.Load(), "concurrent requests share one computation")
	})
}

func TestSnapshotCacheErrorWithoutPreviousValue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cache snapshotCache[int]
		sentinel := errors.New("stats failed")
		_, _, _, err := cache.get(context.Background(), context.Background(), "k", 2*time.Second,
			func(context.Context) (int, error) { return 0, sentinel })
		require.ErrorIs(t, err, sentinel)
	})
}

func TestSnapshotCacheLogsFailedRefreshBehindStaleValue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var logs bytes.Buffer
		cache := snapshotCache[int]{logger: slog.New(slog.NewTextHandler(&logs, nil))}
		_, _, _, err := cache.get(context.Background(), context.Background(), "stats", 2*time.Second,
			func(context.Context) (int, error) { return 1, nil })
		require.NoError(err)

		value, _, stale, err := cache.get(context.Background(), context.Background(), "stats", 2*time.Second,
			func(context.Context) (int, error) { return 0, errors.New("database is locked") })
		require.NoError(err, "a failed refresh with an earlier value serves it")
		assert.Equal(1, value)
		assert.True(stale)
		assert.Contains(logs.String(), "snapshot refresh failed")
		assert.Contains(logs.String(), "key=stats")
		assert.Contains(logs.String(), "database is locked")
	})
}
