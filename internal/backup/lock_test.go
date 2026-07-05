package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func initTestRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	return r
}

func TestExclusiveLockRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)

	data, err := os.ReadFile(r.Path("locks", "exclusive.json"))
	require.NoError(err)
	var info LockInfo
	require.NoError(json.Unmarshal(data, &info))
	assert.Equal("create", info.Operation)
	assert.Equal(os.Getpid(), info.PID)
	assert.NotEmpty(info.Hostname)

	require.NoError(l.Release())
	_, statErr := os.Stat(r.Path("locks", "exclusive.json"))
	assert.True(os.IsNotExist(statErr))
}

func TestExclusiveLockConflicts(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	defer func() { require.NoError(l.Release()) }()

	_, err = r.AcquireExclusiveLock("prune", false)
	require.ErrorIs(err, ErrRepoLocked)
	_, err = r.AcquireSharedLock("verify", false)
	require.ErrorIs(err, ErrRepoLocked)
}

func TestSharedLocksCoexist(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	a, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	b, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	require.NoError(a.Release())
	require.NoError(b.Release())
}

func TestExclusiveWaitsOutSharedLocks(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	shared, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)

	oldTimeout, oldPoll := sharedWaitTimeout, sharedWaitPoll
	sharedWaitTimeout, sharedWaitPoll = 2*time.Second, 20*time.Millisecond
	t.Cleanup(func() { sharedWaitTimeout, sharedWaitPoll = oldTimeout, oldPoll })

	done := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		done <- shared.Release()
	}()

	excl, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	require.NoError(<-done)
	require.NoError(excl.Release())

	// Mirrors the leave-no-file-behind check in
	// TestExclusiveRefusesUnderPersistentShared: once the wait resolves and
	// both locks are released, the locks dir must be clean.
	entries, err := os.ReadDir(r.Path("locks"))
	require.NoError(err)
	assert := assert.New(t)
	assert.Empty(entries)
}

func TestExclusiveRefusesUnderPersistentShared(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	shared, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	defer func() { require.NoError(shared.Release()) }()

	oldTimeout, oldPoll := sharedWaitTimeout, sharedWaitPoll
	sharedWaitTimeout, sharedWaitPoll = 300*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { sharedWaitTimeout, sharedWaitPoll = oldTimeout, oldPoll })

	_, err = r.AcquireExclusiveLock("create", false)
	require.ErrorIs(err, ErrRepoLocked)
	// The failed exclusive attempt must not leave its lock file behind.
	_, statErr := os.Stat(r.Path("locks", "exclusive.json"))
	require.True(os.IsNotExist(statErr))
}

func TestStaleLockIsRemoved(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	l.stopHeartbeat()
	stale := time.Now().Add(-lockStaleAfter - time.Minute)
	require.NoError(os.Chtimes(r.Path("locks", "exclusive.json"), stale, stale))

	l2, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	require.NoError(l2.Release())
}

func TestForceOverridesFreshLock(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	l.stopHeartbeat()

	l2, err := r.AcquireExclusiveLock("create", true)
	require.NoError(err)
	require.NoError(l2.Release())
}

func TestHeartbeatTouchesLockFile(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	oldInterval := lockHeartbeatInterval
	lockHeartbeatInterval = 30 * time.Millisecond
	t.Cleanup(func() { lockHeartbeatInterval = oldInterval })

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	defer func() { require.NoError(l.Release()) }()

	past := time.Now().Add(-time.Hour)
	require.NoError(os.Chtimes(r.Path("locks", "exclusive.json"), past, past))
	require.Eventually(func() bool {
		info, statErr := os.Stat(r.Path("locks", "exclusive.json"))
		return statErr == nil && time.Since(info.ModTime()) < time.Minute
	}, 2*time.Second, 10*time.Millisecond)
}

// TestHeartbeatStopsAfterReplant pins the fix for the heartbeat goroutine
// refreshing a lock file's mtime unconditionally. If this holder is reaped
// as stale while still running and a successor replants the same path with
// its own LockInfo, the original holder's heartbeat must detect the mismatch
// on its next tick and stop touching the file, rather than keeping the
// successor's lock artificially fresh forever.
func TestHeartbeatStopsAfterReplant(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	oldInterval := lockHeartbeatInterval
	lockHeartbeatInterval = 30 * time.Millisecond
	t.Cleanup(func() { lockHeartbeatInterval = oldInterval })

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	t.Cleanup(func() { _ = os.Remove(l.path) })

	// Simulate a stale reap-and-replant: a successor's LockInfo now occupies
	// l.path even though l's heartbeat goroutine is still running.
	successor := LockInfo{
		Hostname:   "successor-host",
		PID:        99998,
		Operation:  "prune",
		AcquiredAt: time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(successor)
	require.NoError(err)
	require.NoError(os.WriteFile(l.path, body, 0o600))
	past := time.Now().Add(-time.Hour)
	require.NoError(os.Chtimes(l.path, past, past))

	// Give the heartbeat goroutine several ticks' worth of time to observe
	// the mismatch and stop.
	time.Sleep(10 * lockHeartbeatInterval)

	info, statErr := os.Stat(l.path)
	require.NoError(statErr)
	require.Equal(past.Unix(), info.ModTime().Unix(),
		"heartbeat must not refresh a replanted lock's mtime")

	l.stopHeartbeat()
}

// TestSharedLockPostCreateRecheckDetectsExclusive pins the create-then-verify
// handshake that closes the TOCTOU race between AcquireSharedLock's pre-plant
// check and AcquireExclusiveLock's plant. There is no exclusive.json when
// AcquireSharedLock starts, so the pre-plant check passes; sharedLockPostPlantHook
// then hand-plants a fresh exclusive.json, standing in for an exclusive
// acquirer that finished planting and scanning in the window between the
// shared acquirer's pre-check and its own plant. Without the post-create
// re-check, AcquireSharedLock would succeed and leave its shared-<ulid>.json
// file behind even though a fresh exclusive lock now exists; the fix must
// refuse and clean up after itself, leaving the hand-planted exclusive lock
// in place.
func TestSharedLockPostCreateRecheckDetectsExclusive(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	oldHook := sharedLockPostPlantHook
	t.Cleanup(func() { sharedLockPostPlantHook = oldHook })
	sharedLockPostPlantHook = func() {
		info := LockInfo{
			Hostname:   "racer-host",
			PID:        99999,
			Operation:  "create",
			AcquiredAt: time.Now().UTC().Format(time.RFC3339),
		}
		body, err := json.Marshal(info)
		require.NoError(err)
		require.NoError(os.WriteFile(r.Path("locks", "exclusive.json"), body, 0o600))
	}

	_, err := r.AcquireSharedLock("verify", false)
	require.ErrorIs(err, ErrRepoLocked)

	entries, err := os.ReadDir(r.Path("locks"))
	require.NoError(err)
	for _, e := range entries {
		require.False(strings.HasPrefix(e.Name(), "shared-"),
			"shared lock file left behind: %s", e.Name())
	}

	_, statErr := os.Stat(r.Path("locks", "exclusive.json"))
	require.NoError(statErr, "hand-planted exclusive lock should still exist")
}

// TestReleaseDoesNotRemoveReplantedLock pins the fix for Release() removing
// whatever is at its path unconditionally. If holder A's lock is reaped as
// stale (A was slow, not dead) and holder B replants exclusive.json at the
// same path, A's later Release must not delete B's live lock.
func TestReleaseDoesNotRemoveReplantedLock(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	a, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	a.stopHeartbeat()
	stale := time.Now().Add(-lockStaleAfter - time.Minute)
	require.NoError(os.Chtimes(r.Path("locks", "exclusive.json"), stale, stale))

	b, err := r.AcquireExclusiveLock("prune", false)
	require.NoError(err)

	require.NoError(a.Release())

	_, statErr := os.Stat(r.Path("locks", "exclusive.json"))
	require.NoError(statErr, "B's lock file should still exist after A.Release()")

	require.NoError(b.Release())
}

// TestReleaseReturnsErrorOnUnreadableLockFile pins the distinction in
// Release's ownership re-read: os.ErrNotExist means the lock is already
// gone (not our error), but any other read failure must surface as an
// error instead of being swallowed, since we cannot tell whether we still
// own the lock.
func TestReleaseReturnsErrorOnUnreadableLockFile(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	l, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	l.stopHeartbeat()

	require.NoError(os.Remove(l.path))
	require.NoError(os.Mkdir(l.path, 0o700))
	t.Cleanup(func() { _ = os.Remove(l.path) })

	err = l.Release()
	require.Error(err)
	require.NotErrorIs(err, os.ErrNotExist)
}
