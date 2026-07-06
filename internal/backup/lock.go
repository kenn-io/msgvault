package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/pack"
)

// ErrRepoLocked reports that another operation holds a conflicting repo lock.
var ErrRepoLocked = errors.New("backup: repository is locked")

var (
	lockStaleAfter        = 30 * time.Minute
	lockHeartbeatInterval = 30 * time.Second
	sharedWaitTimeout     = 60 * time.Second
	sharedWaitPoll        = 200 * time.Millisecond
)

const exclusiveLockName = "exclusive.json"

// sharedLockPostPlantHook runs between planting a shared lock file and the
// exclusive re-check; tests use it to open the race window deterministically.
var sharedLockPostPlantHook = func() {}

// LockInfo is the JSON body of a repo lock file. Freshness is carried by the
// file's mtime (heartbeat), not by fields, so observers need no clock sync.
type LockInfo struct {
	Hostname   string `json:"hostname"`
	PID        int    `json:"pid"`
	Operation  string `json:"operation"`
	AcquiredAt string `json:"acquired_at"`
}

// RepoLock is a held repository lock with a heartbeat goroutine. info holds
// the exact LockInfo this process wrote, so Release can verify it still owns
// the file at path before removing it (the file may have been reaped as
// stale and replanted by another holder in the meantime).
type RepoLock struct {
	path string
	info LockInfo
	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// AcquireExclusiveLock takes locks/exclusive.json for a mutating operation.
// It removes stale locks, refuses fresh ones unless force is set, and after
// planting the exclusive file waits out fresh shared locks (releasing and
// failing if they persist past sharedWaitTimeout).
func (r *Repo) AcquireExclusiveLock(operation string, force bool) (*RepoLock, error) {
	path := r.Path(locksDirName, exclusiveLockName)
	if err := clearConflicting(path, force); err != nil {
		return nil, err
	}
	lock, err := plantLock(path, operation)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(sharedWaitTimeout)
	for {
		holders, err := r.freshSharedLocks(force)
		if err != nil {
			_ = lock.Release()
			return nil, err
		}
		if len(holders) == 0 {
			return lock, nil
		}
		if time.Now().After(deadline) {
			_ = lock.Release()
			return nil, fmt.Errorf(
				"%w: shared lock(s) held by %s",
				ErrRepoLocked,
				strings.Join(holders, ", "),
			)
		}
		time.Sleep(sharedWaitPoll)
	}
}

// AcquireSharedLock takes locks/shared-<ulid>.json for a read-walking
// operation (verify, restore). It refuses under a fresh exclusive lock.
//
// The pre-plant check alone is racy: AcquireExclusiveLock could plant
// exclusive.json and finish its (single) freshSharedLocks scan in the window
// between our check and our own plant, and both sides would then believe
// they hold a compatible lock. Closing that requires the standard
// create-then-verify handshake: after planting our shared file we re-check
// for a fresh exclusive lock and back off if one is now present. This is
// safe for the mirrored ordering too — if our shared file lands first,
// AcquireExclusiveLock's freshSharedLocks scan (which always runs after its
// own plant) will see it and wait.
func (r *Repo) AcquireSharedLock(operation string, force bool) (*RepoLock, error) {
	exclusive := r.Path(locksDirName, exclusiveLockName)
	if err := clearConflicting(exclusive, force); err != nil {
		return nil, err
	}
	name := "shared-" + pack.NewPackID() + ".json"
	lock, err := plantLock(r.Path(locksDirName, name), operation)
	if err != nil {
		return nil, err
	}
	sharedLockPostPlantHook()
	if err := clearConflicting(exclusive, force); err != nil {
		_ = lock.Release()
		return nil, err
	}
	return lock, nil
}

// clearConflicting removes path if it is stale (or force is set); it returns
// ErrRepoLocked if a fresh lock remains.
func clearConflicting(path string, force bool) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(
			"backup: checking lock %s: %w",
			filepath.Base(path),
			err,
		)
	}
	if force || time.Since(info.ModTime()) > lockStaleAfter {
		if err := os.Remove(path); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"backup: removing lock %s: %w",
				filepath.Base(path),
				err,
			)
		}
		return nil
	}
	return fmt.Errorf(
		"%w: %s held by %s",
		ErrRepoLocked,
		filepath.Base(path),
		describeLock(path),
	)
}

func (r *Repo) freshSharedLocks(force bool) ([]string, error) {
	entries, err := os.ReadDir(r.Path(locksDirName))
	if err != nil {
		return nil, fmt.Errorf("backup: reading locks dir: %w", err)
	}
	var holders []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "shared-") {
			continue
		}
		path := r.Path(locksDirName, e.Name())
		info, err := e.Info()
		if err != nil {
			continue // lock vanished between readdir and stat
		}
		if force || time.Since(info.ModTime()) > lockStaleAfter {
			_ = os.Remove(path)
			continue
		}
		holders = append(holders, describeLock(path))
	}
	return holders, nil
}

func plantLock(path, operation string) (*RepoLock, error) {
	hostname, _ := os.Hostname()
	info := LockInfo{
		Hostname:   hostname,
		PID:        os.Getpid(),
		Operation:  operation,
		AcquiredAt: time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("backup: encoding lock info: %w", err)
	}
	f, err := os.OpenFile(
		path,
		os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		0o600,
	)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf(
				"%w: %s held by %s",
				ErrRepoLocked,
				filepath.Base(path),
				describeLock(path),
			)
		}
		return nil, fmt.Errorf(
			"backup: creating lock %s: %w",
			filepath.Base(path),
			err,
		)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf(
			"backup: writing lock %s: %w",
			filepath.Base(path),
			err,
		)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf(
			"backup: closing lock %s: %w",
			filepath.Base(path),
			err,
		)
	}
	l := &RepoLock{path: path, info: info, stop: make(chan struct{})}
	l.wg.Add(1)
	go l.heartbeat()
	return l, nil
}

func (l *RepoLock) heartbeat() {
	defer l.wg.Done()
	ticker := time.NewTicker(lockHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			// A holder can be reaped as stale (clearConflicting or
			// freshSharedLocks removing it) and replanted by a successor
			// while this goroutine is still running, e.g. a slow or
			// briefly-descheduled process. Refreshing the file's mtime in
			// that case would keep the successor's lock artificially fresh
			// forever. Re-read and compare identity every tick, matching
			// Release's ownership check, and stop for good on any mismatch
			// or read error (including the file having vanished).
			current, ok, err := currentLockInfo(l.path)
			if err != nil || !ok || current != l.info {
				return
			}
			now := time.Now()
			_ = os.Chtimes(l.path, now, now)
		}
	}
}

func (l *RepoLock) stopHeartbeat() {
	l.once.Do(func() { close(l.stop) })
	l.wg.Wait()
}

// currentLockInfo reads and parses the LockInfo currently stored at path.
// ok is false whenever the file cannot be trusted to represent a live lock
// this process still owns: it is missing, unreadable, or fails to parse.
// err is set only when the file exists but could not be read, so a caller
// wanting to distinguish "definitely not ours" from "we don't know" (as
// Release does) can still surface a real I/O failure; a missing file or an
// unparsable body are reported as ok == false, err == nil.
func currentLockInfo(path string) (info LockInfo, ok bool, err error) {
	data, readErr := os.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return LockInfo{}, false, nil
	}
	if readErr != nil {
		return LockInfo{}, false, readErr
	}
	if parseErr := json.Unmarshal(data, &info); parseErr != nil {
		return LockInfo{}, false, nil //nolint:nilerr // unparsable body: reported as ok == false, not an error
	}
	return info, true, nil
}

// Release stops the heartbeat and removes the lock file, but only if the
// file still holds the LockInfo this RepoLock planted. If this holder was
// slow enough to be reaped as stale, another holder may have replanted the
// same path with its own live lock; removing unconditionally would delete
// that lock out from under it. When the contents don't match ours (or can't
// be read), our lock is already gone, which is not an error for the
// releaser. This ownership check is itself a read-compare-remove window: a
// replant landing between our read and our remove is not observable here.
func (l *RepoLock) Release() error {
	l.stopHeartbeat()
	current, ok, err := currentLockInfo(l.path)
	if err != nil {
		return fmt.Errorf(
			"backup: could not verify lock %s for removal: %w",
			filepath.Base(l.path),
			err,
		)
	}
	if !ok || current != l.info {
		return nil // gone, unparsable, or replanted: our lock is already gone
	}
	if err := os.Remove(l.path); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"backup: releasing lock %s: %w",
			filepath.Base(l.path),
			err,
		)
	}
	return nil
}

func describeLock(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown holder"
	}
	var info LockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return "unknown holder"
	}
	return fmt.Sprintf(
		"%s pid %d (%s since %s)",
		info.Hostname,
		info.PID,
		info.Operation,
		info.AcquiredAt,
	)
}
