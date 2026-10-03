package requestsign

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/fileutil"
)

const StartupQuarantine = 36 * time.Second
const clockTolerance = 100 * time.Millisecond
const clockContinuity = time.Second

var (
	ErrReplayUnavailable = errors.New("replay protection unavailable")
	ErrReplay            = errors.New("replayed or expired request")
)

type replayState struct {
	Version   int   `json:"version"`
	MaxExpiry int64 `json:"max_expiry"`
}

// ReplayGuard keeps bounded live nonces in memory. Persisting their maximum
// expiry before execution makes every pre-restart signature stale before the
// new verifier leaves quarantine. The stable sibling lock is never replaced.
type ReplayGuard struct {
	mu            sync.Mutex
	path          string
	lock          *flock.Flock
	monotonic     func(time.Time) time.Duration
	wallClock     func(time.Time) time.Time
	startupExpiry int64
	maxExpiry     int64
	capacity      int
	nonces        map[string]int64
	closed        bool
	poisoned      bool
	lastWall      int64
	highWall      int64
	lastMono      time.Duration
	fenced        bool
	barrier       int64
	stableSince   time.Duration
	anchorWall    int64
	anchorMono    time.Duration
}

func InitReplayState(path string) error {
	file, err := fileutil.SecureOpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("initialize replay state: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	return persistReplayState(path, 0)
}

func persistReplayState(path string, expiry int64) error {
	data, err := json.Marshal(replayState{Version: 1, MaxExpiry: expiry})
	if err != nil {
		return err
	}
	return fileutil.SecureReplaceFile(path, append(data, '\n'), 0o600)
}

func OpenReplayGuard(path string, boot time.Time, capacity int) (*ReplayGuard, error) {
	if capacity < 1 || capacity > 100000 || path == "" {
		return nil, ErrReplayUnavailable
	}
	// Private parent ownership prevents another user replacing lock/state names.
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("replay state parent: %w", err)
	}
	info, statErr := parent.Stat()
	if statErr == nil {
		statErr = privateFilePermissions(parent, info)
	}
	_ = parent.Close()
	if statErr != nil {
		return nil, fmt.Errorf("replay state directory must be private: %w", statErr)
	}
	lockPath := path + ".lock"
	file, err := fileutil.SecureOpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		err = file.Close()
	} else if errors.Is(err, os.ErrExist) {
		_, err = ReadPrivateFile(lockPath, 1024)
	}
	if err != nil {
		return nil, fmt.Errorf("replay lock: %w", err)
	}
	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		return nil, errors.New("replay state is already owned or cannot be locked")
	}
	failed := true
	defer func() {
		if failed {
			_ = lock.Close()
		}
	}()
	data, err := ReadPrivateFile(path, 1024)
	if err != nil {
		return nil, fmt.Errorf("existing replay state required: %w", err)
	}
	var decoded struct {
		Version   *int   `json:"version"`
		MaxExpiry *int64 `json:"max_expiry"`
	}
	if err := json.Unmarshal(data, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return nil, ErrReplayUnavailable
	}
	if decoded.Version == nil || decoded.MaxExpiry == nil || *decoded.Version != 1 || *decoded.MaxExpiry < 0 || *decoded.MaxExpiry > 999999999999 {
		return nil, ErrReplayUnavailable
	}
	state := replayState{Version: *decoded.Version, MaxExpiry: *decoded.MaxExpiry}
	guard := &ReplayGuard{path: path, lock: lock, startupExpiry: state.MaxExpiry, maxExpiry: state.MaxExpiry, capacity: capacity, nonces: make(map[string]int64), lastWall: boot.UnixNano(), highWall: boot.UnixNano(), anchorWall: boot.UnixNano()}
	guard.monotonic = func(now time.Time) time.Duration { return now.Sub(boot) }
	guard.wallClock = func(time.Time) time.Time { return time.Now() }
	failed = false
	return guard, nil
}

func (g *ReplayGuard) readyLocked(now time.Time) error {
	if g.closed || g.poisoned {
		return ErrReplayUnavailable
	}
	mono := g.monotonic(now)
	wall := now.UnixNano()
	fault := mono < g.lastMono || wall < g.lastWall || time.Duration(wall-g.anchorWall)+clockTolerance < mono-g.anchorMono
	if fault {
		if !g.fenced {
			g.barrier = max(g.highWall, time.Unix(g.maxExpiry, 0).UnixNano())
		}
		g.fenced = true
		g.stableSince = mono
		g.anchorWall = wall
		g.anchorMono = mono
	}
	g.lastMono = mono
	g.lastWall = wall
	g.highWall = max(g.highWall, wall)
	if g.fenced {
		if wall <= g.barrier || mono-g.stableSince < clockContinuity {
			return ErrReplayUnavailable
		}
		g.fenced = false
		g.anchorWall = wall
		g.anchorMono = mono
	} else if mono-g.anchorMono >= clockContinuity {
		g.anchorWall = wall
		g.anchorMono = mono
	}
	if mono < StartupQuarantine || now.Unix() <= g.startupExpiry {
		return ErrReplayUnavailable
	}
	return nil
}

func (g *ReplayGuard) Ready(now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.readyLocked(g.wallClock(now))
}

// Use atomically consumes a nonce and durably covers its expiry before returning.
// Caller must recheck freshness, key validity and cancellation after fsync and
// before dispatch. A persistence failure poisons this guard until it is closed.
func (g *ReplayGuard) Use(key, nonce string, expiry, now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now = g.wallClock(now)
	if err := g.readyLocked(now); err != nil {
		return err
	}
	if !expiry.After(now) {
		return ErrReplay
	}
	for id, end := range g.nonces {
		if end <= now.Unix() {
			delete(g.nonces, id)
		}
	}
	id := key + "\x00" + nonce
	if _, ok := g.nonces[id]; ok {
		return ErrReplay
	}
	if len(g.nonces) >= g.capacity {
		return ErrReplayUnavailable
	}
	end := expiry.Unix()
	if end > g.maxExpiry {
		if err := persistReplayState(g.path, end); err != nil {
			g.poisoned = true
			return ErrReplayUnavailable
		}
		g.maxExpiry = end
	}
	g.nonces[id] = end
	return nil
}

func (g *ReplayGuard) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	if err := g.lock.Close(); err != nil {
		return fmt.Errorf("close replay lock: %w", err)
	}
	return nil
}

// Fence stops admission while retaining the exclusive process lock.
func (g *ReplayGuard) Fence() { g.mu.Lock(); g.poisoned = true; g.mu.Unlock() }
