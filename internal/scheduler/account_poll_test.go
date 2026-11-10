package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
)

func TestPollingAccountStartsAndRepeats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[[accounts]]\nemail = 'a@example.com'\nenabled = true\nschedule = '0 * * * *'\npoll_interval = '15s'\n"), 0600))
	cfg, err := config.Load(path, "")
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		var calls int
		s := New(func(context.Context, string) error { return nil }).WithAccountPoll(func(context.Context, string) error { calls++; return nil })
		n, errs := s.AddAccountsFromConfig(cfg)
		require.Empty(t, errs)
		require.Equal(t, 1, n)
		s.Start()
		s.Start()
		synctest.Wait()
		assert.Equal(t, 1, calls)
		assert.Equal(t, 15*time.Second, s.Status()[0].NextRun.Sub(time.Now()))
		time.Sleep(15 * time.Second)
		synctest.Wait()
		assert.Equal(t, 2, calls)
		<-s.Stop().Done()
	})
}

func TestIncompletePollingPreservesCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		s := New(func(context.Context, string) error { return nil }).WithAccountPoll(func(context.Context, string) error {
			calls++
			if calls == 2 {
				return ErrAcquisitionIncomplete
			}
			return nil
		})
		_, errs := s.AddAccountsFromConfig(&config.Config{Accounts: []config.AccountSchedule{{Email: "a@example.com", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}}})
		require.Empty(t, errs)
		s.Start()
		synctest.Wait()
		first := s.Status()[0].LastCompleted
		time.Sleep(15 * time.Second)
		synctest.Wait()
		status := s.Status()[0]
		assert.Equal(t, first, status.LastCompleted)
		assert.True(t, status.LastAttempted.After(first))
		assert.Empty(t, status.LastError)
		s.RemoveAccount("a@example.com")
		time.Sleep(15 * time.Second)
		synctest.Wait()
		assert.Equal(t, 2, calls)
		<-s.Stop().Done()
	})
}

func TestCronPromotesQueuedPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := newSerialWorkTracker()
		release, ok := gate.BeginWorkContext(t.Context())
		require.True(t, ok)
		var cronCalls, pollCalls int
		s := New(func(context.Context, string) error { cronCalls++; return nil }).WithAccountPoll(func(context.Context, string) error { pollCalls++; return nil }).WithWorkTracker(gate)
		require.NoError(t, s.AddAccount("a@example.com", "0 * * * *"))
		go s.onAccountTrigger("a@example.com", true)
		synctest.Wait()
		assert.False(t, s.hasQueuedRuns())
		s.onAccountTick("a@example.com")
		assert.True(t, s.hasQueuedRuns())
		release()
		synctest.Wait()
		assert.Equal(t, 1, cronCalls)
		assert.Zero(t, pollCalls)
		assert.False(t, s.hasQueuedRuns())
		<-s.Stop().Done()
	})
}
