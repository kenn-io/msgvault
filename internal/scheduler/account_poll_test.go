package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestAccountPollingStartupCatchup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		require.NoError(t, os.WriteFile(path, []byte(`[[accounts]]
email = "sample@example.test"
schedule = "0 0 1 1 *"
enabled = true
poll_interval = "30s"
`), 0o600))
		cfg, err := config.Load(path, dir)
		require.NoError(t, err)
		var calls atomic.Int32
		s := New(func(context.Context, string) error { calls.Add(1); return nil })
		n, errs := s.AddAccountsFromConfig(cfg)
		require.Empty(t, errs)
		require.Equal(t, 1, n)
		assert.Zero(t, calls.Load(), "registration must not run before Start")
		s.Start()
		synctest.Wait()
		assert.EqualValues(t, 1, calls.Load(), "rapid accounts catch up when the scheduler starts")
		time.Sleep(30 * time.Second)
		synctest.Wait()
		assert.EqualValues(t, 2, calls.Load(), "typed polling joins the account runner")
		<-s.Stop().Done()
	})
}

func pollingSchedulerFixture(t *testing.T, run SyncFunc) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[[accounts]]
email = "sample@example.test"
schedule = "0 0 1 1 *"
enabled = true
poll_interval = "30s"
`), 0o600))
	cfg, err := config.Load(path, dir)
	require.NoError(t, err)
	s := New(run).WithWorkTracker(newSerialWorkTracker())
	_, errs := s.AddAccountsFromConfig(cfg)
	require.Empty(t, errs)
	return s
}

func TestAccountPollingFailureDoesNotImmediatelyReplayQueuedPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		s := pollingSchedulerFixture(t, func(context.Context, string) error {
			if calls.Add(1) == 1 {
				<-release
			}
			return context.DeadlineExceeded
		})
		s.Start()
		synctest.Wait()
		time.Sleep(30 * time.Second) // a poll arrives while acquisition is executing
		synctest.Wait()
		close(release)
		synctest.Wait()
		assert.EqualValues(t, 1, calls.Load(), "a pending poll must honor the failure's new backoff")
		time.Sleep(30 * time.Second)
		synctest.Wait()
		assert.EqualValues(t, 1, calls.Load(), "first failure backs off beyond the ordinary interval")
		<-s.Stop().Done()
	})
}

func TestAccountPollingOrdinaryIntentSurvivesPollAndFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		s := pollingSchedulerFixture(t, func(context.Context, string) error {
			if calls.Add(1) == 1 {
				<-release
				return context.DeadlineExceeded
			}
			return nil
		})
		s.Start()
		synctest.Wait()
		s.onAccountTick("sample@example.test")
		time.Sleep(30 * time.Second)
		synctest.Wait()
		close(release)
		synctest.Wait()
		assert.EqualValues(t, 2, calls.Load(), "polls must not erase an accepted ordinary follow-up")
		<-s.Stop().Done()
	})
}
