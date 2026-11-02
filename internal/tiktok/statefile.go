package tiktok

import (
	"context"
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

func withFileLock(ctx context.Context, path string, fn func() error) (err error) {
	if err := fileutil.SecureMkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock := flock.New(path+".lock", flock.SetPermissions(0600))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock TikTok state: %w", err)
	}
	if !locked {
		return ctx.Err()
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	return fn()
}

func readState(path string, value any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if json.Unmarshal(data, value) != nil {
		return errors.New("invalid TikTok state file")
	}
	return nil
}
func writeState(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return fileutil.SecureReplaceFile(path, data, 0600)
}

type rateState struct {
	Next         time.Time `json:"next"`
	BlockedUntil time.Time `json:"blocked_until"`
}

// Limiter spaces reservations by 100 ms: no bursts beyond 10/second or
// 600/minute. Its optional file coordinates daemon jobs and subprocesses.
type Limiter struct {
	mu    sync.Mutex
	path  string
	state rateState
	now   func() time.Time
	wait  func(context.Context, time.Duration) error
}

func NewLimiter(path string) *Limiter { return &Limiter{path: path, now: time.Now, wait: waitContext} }

func (l *Limiter) update(ctx context.Context, fn func(*rateState) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path == "" {
		return fn(&l.state)
	}
	return withFileLock(ctx, l.path, func() error {
		var state rateState
		if err := readState(l.path, &state); err != nil {
			return err
		}
		if err := fn(&state); err != nil {
			return err
		}
		return writeState(l.path, state)
	})
}
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var delay time.Duration
		err := l.update(ctx, func(state *rateState) error {
			now := l.now()
			if state.BlockedUntil.After(now) {
				return ErrRateLimited
			}
			delay = state.Next.Sub(now)
			if delay <= 0 {
				state.Next = now.Add(100 * time.Millisecond)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if delay <= 0 {
			return nil
		}
		if err := l.wait(ctx, delay); err != nil {
			return err
		}
	}
}
func (l *Limiter) Block(ctx context.Context) error {
	return l.update(ctx, func(state *rateState) error { state.BlockedUntil = l.now().Add(5 * time.Minute); return nil })
}
