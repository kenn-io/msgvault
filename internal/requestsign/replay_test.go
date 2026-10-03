package requestsign

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/fileutil"
)

func replayFixture(t *testing.T, capacity int) (*ReplayGuard, time.Time, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, fileutil.SecureChmod(dir, 0o700))
	path := filepath.Join(dir, "replay.json")
	require.NoError(t, InitReplayState(path))
	boot := time.Now()
	guard, err := fixtureOpenReplayGuard(path, boot, capacity)
	require.NoError(t, err)
	t.Cleanup(func() { _ = guard.Close() })
	return guard, boot, path
}

func TestReplayConcurrentCapacityExpiryAndRestart(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	guard, boot, path := replayFixture(t, 1)
	requirements.Error(guard.Ready(boot))
	ready := boot.Add(36 * time.Second)
	requirements.NoError(guard.Ready(ready))
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if guard.Use("key", "nonce", ready.Add(30*time.Second), ready) == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	assertions.Equal(int64(1), accepted.Load())
	requirements.Error(guard.Use("key", "other", ready.Add(30*time.Second), ready))
	requirements.NoError(guard.Close())
	restarted, err := fixtureOpenReplayGuard(path, ready, 100)
	requirements.NoError(err)
	defer func() { require.NoError(t, restarted.Close()) }()
	requirements.Error(restarted.Use("key", "nonce", ready.Add(30*time.Second), ready.Add(time.Second)))
	requirements.Error(restarted.Use("key", "nonce", ready.Add(30*time.Second), ready.Add(36*time.Second)))
	assertions.NoError(restarted.Use("key", "new", ready.Add(67*time.Second), ready.Add(36*time.Second)))
}

func TestReplayStateMissingCorruptLockAndRollback(t *testing.T) {
	requirements := require.New(t)

	guard, boot, path := replayFixture(t, 100)
	_, err := OpenReplayGuard(path, boot, 100)
	requirements.Error(err)
	ready := boot.Add(36 * time.Second)
	requirements.NoError(guard.Use("key", "nonce", ready.Add(30*time.Second), ready))
	_, err = OpenReplayGuard(path, ready, 100)
	requirements.Error(err, "stable lock must survive state replacement")
	requirements.Error(guard.Use("key", "backward", ready.Add(30*time.Second), boot))
	requirements.NoError(guard.Close())
	restarted, err := fixtureOpenReplayGuard(path, boot.Add(-time.Hour), 100)
	requirements.NoError(err)
	requirements.Error(restarted.Ready(boot.Add(-time.Hour + 36*time.Second)))
	requirements.NoError(restarted.Close())
	requirements.NoError(os.WriteFile(path, []byte("corrupt"), 0o600))
	_, err = OpenReplayGuard(path, boot, 100)
	requirements.Error(err)
	_, err = OpenReplayGuard(filepath.Join(t.TempDir(), "missing"), boot, 100)
	requirements.Error(err)
	requirements.Error(InitReplayState(path))
}

func TestReplayLateBodyCannotReserveExpiredNonce(t *testing.T) {
	guard, boot, _ := replayFixture(t, 100)
	ready := boot.Add(36 * time.Second)
	expiry := ready.Add(30 * time.Second)
	require.NoError(t, guard.Use("key", "nonce", expiry, ready))
	require.Error(t, guard.Use("key", "nonce", expiry, expiry))
	require.Error(t, guard.Use("key", "other", expiry, expiry))
}

func TestReplayRollbackAfterEvictionAndRestart(t *testing.T) {
	requirements := require.New(t)

	guard, boot, path := replayFixture(t, 1)
	ready := boot.Add(36 * time.Second)
	requirements.NoError(guard.Use("key", "old", ready.Add(30*time.Second), ready))
	later := ready.Add(31 * time.Second)
	requirements.NoError(guard.Use("key", "new", later.Add(30*time.Second), later))
	// The old nonce was evicted. Wallclock rollback must not make its still
	// authentic signature usable again, in-process or after reopening state.
	requirements.Error(guard.Use("key", "old", ready.Add(30*time.Second), ready.Add(15*time.Second)))
	requirements.NoError(guard.Close())
	restarted, err := fixtureOpenReplayGuard(path, ready.Add(-time.Hour), 100)
	requirements.NoError(err)
	defer func() { require.NoError(t, restarted.Close()) }()
	requirements.Error(restarted.Ready(ready.Add(-time.Hour + 36*time.Second)))
}

func TestReplayPersistenceFailurePoisonsGuard(t *testing.T) {
	requirements := require.New(t)

	guard, boot, path := replayFixture(t, 10)
	parent := filepath.Dir(path)
	moved := parent + "-moved"
	requirements.NoError(os.Rename(parent, moved))
	t.Cleanup(func() { _ = os.Rename(moved, parent) })
	ready := boot.Add(36 * time.Second)
	requirements.Error(guard.Use("key", "first", ready.Add(30*time.Second), ready))
	requirements.NoError(os.Rename(moved, parent))
	requirements.Error(guard.Use("key", "second", ready.Add(30*time.Second), ready), "restoring disk access cannot silently reopen admission")
}

// A private monotonic seam keeps wall and elapsed observations independent.
// Production uses time.Since(boot), never the caller's wallclock argument.
func fixtureOpenReplayGuard(path string, boot time.Time, capacity int) (*ReplayGuard, error) {
	guard, err := OpenReplayGuard(path, boot, capacity)
	if err == nil {
		guard.monotonic = func(now time.Time) time.Duration { return now.Sub(boot) }
		guard.wallClock = func(now time.Time) time.Time { return now }
	}
	return guard, err
}

func TestReplayFrozenWallRepeatedFaultAndRecovery(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	guard, boot, _ := replayFixture(t, 10)
	elapsed := 36 * time.Second
	guard.monotonic = func(time.Time) time.Duration { return elapsed }
	ready := boot.Add(36 * time.Second)
	require.NoError(t, guard.Use("key", "first", ready.Add(30*time.Second), ready))
	elapsed += 2 * time.Second
	requirements.Error(guard.Ready(ready), "frozen wall with advancing monotonic time must fence")
	elapsed += 2 * time.Second
	requirements.Error(guard.Ready(ready), "a repeated frozen sample cannot recover")
	elapsed += 31 * time.Second
	recovered := ready.Add(31 * time.Second)
	requirements.NoError(guard.Ready(recovered), "progress and expired durable barrier permit recovery")
	requirements.Error(guard.Use("key", "first", ready.Add(30*time.Second), recovered))
	assertions.NoError(guard.Use("key", "new", recovered.Add(30*time.Second), recovered))
}

func TestReplayProductionSamplesClockInsideAdmissionLock(t *testing.T) {
	requirements := require.New(t)

	dir := t.TempDir()
	requirements.NoError(fileutil.SecureChmod(dir, 0o700))
	path := filepath.Join(dir, "state.json")
	requirements.NoError(InitReplayState(path))
	boot := time.Now().Add(-40 * time.Second)
	guard, err := OpenReplayGuard(path, boot, 10)
	requirements.NoError(err)
	defer func() { require.NoError(t, guard.Close()) }()
	capturedEarlier := time.Now()
	requirements.NoError(guard.Ready(time.Now().Add(time.Second)))
	requirements.NoError(guard.Use("key", "nonce", time.Now().Add(30*time.Second), capturedEarlier), "out-of-order callers must not look like a wallclock rollback")
}

func TestReplayFrequentFrozenSamplesCannotResetClockTolerance(t *testing.T) {
	guard, boot, _ := replayFixture(t, 10)
	elapsed := 36 * time.Second
	guard.monotonic = func(time.Time) time.Duration { return elapsed }
	ready := boot.Add(36 * time.Second)
	require.NoError(t, guard.Ready(ready))
	var fenced bool
	for range 20 {
		elapsed += 20 * time.Millisecond
		if guard.Ready(ready) != nil {
			fenced = true
		}
	}
	assert.True(t, fenced, "frequent sub-tolerance samples must accumulate frozen-wall deficit")
}

func TestReplayProductionClockUsesOneAdmissionInstant(t *testing.T) {
	requirements := require.New(t)

	dir := t.TempDir()
	requirements.NoError(fileutil.SecureChmod(dir, 0o700))
	path := filepath.Join(dir, "state.json")
	requirements.NoError(InitReplayState(path))
	boot := time.Now()
	guard, err := OpenReplayGuard(path, boot, 10)
	requirements.NoError(err)
	defer func() { require.NoError(t, guard.Close()) }()
	// A wall-clock sample carries its matching monotonic timestamp. The caller
	// may be descheduled after this read; no second timestamp may change admission.
	sample := boot.Add(36 * time.Second)
	guard.wallClock = func(time.Time) time.Time { return sample }
	assert.NoError(t, guard.Ready(boot))
}

func TestReplayStateRequiresCompleteUnambiguousWatermark(t *testing.T) {
	for _, state := range []string{
		`{"version":1}`,
		`{"version":1,"max_expiry":null}`,
		`{"version":1,"max_expiry":1791000030,"max_expiry":0}`,
		`{"version":1,"version":1,"max_expiry":0}`,
	} {
		t.Run(state, func(t *testing.T) {
			requirements := require.New(t)

			dir := t.TempDir()
			requirements.NoError(fileutil.SecureChmod(dir, 0o700))
			path := filepath.Join(dir, "state.json")
			requirements.NoError(fileutil.SecureWriteFile(path, []byte(state), 0o600))
			guard, err := OpenReplayGuard(path, time.Now(), 10)
			if guard != nil {
				requirements.NoError(guard.Close())
			}
			require.ErrorIs(t, err, ErrReplayUnavailable, "incomplete or duplicate state cannot reset the replay watermark")
		})
	}
}
