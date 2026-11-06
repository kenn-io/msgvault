package meetingimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

func RecorderSyncPaths(dbPath, lockDir string) (canonical, dir string, err error) {
	return recorderPaths(dbPath, lockDir, filepath.EvalSymlinks)
}

func recorderPaths(dbPath, lockDir string, resolve func(string) (string, error)) (canonical, dir string, err error) {
	canonical, err = filepath.Abs(dbPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve meeting root: %w", err)
	}
	canonical, err = resolve(canonical)
	if err != nil {
		return "", "", fmt.Errorf("resolve meeting root: %w", err)
	}
	if lockDir == "" {
		return "", "", errors.New("meeting remote sync requires a lock directory")
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create meeting sync lock directory: %w", err)
	}
	return canonical, lockDir, nil
}

// resolveMissingRoot keys a root that is offline or not yet created by its
// resolved nearest existing ancestor, the key the root gets once it appears
// as a directory. A dangling symlink still fails: its target key is unknown.
func resolveMissingRoot(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if !errors.Is(err, os.ErrNotExist) {
		return resolved, err
	}
	parent := filepath.Dir(path)
	if _, lerr := os.Lstat(path); !errors.Is(lerr, os.ErrNotExist) || parent == path {
		return "", err
	}
	if resolved, err = resolveMissingRoot(parent); err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

func LockRecorderSource(ctx context.Context, canonical, lockDir string) (*flock.Flock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(canonical))
	lock := flock.New(filepath.Join(lockDir, hex.EncodeToString(hash[:])+".lock"), flock.SetPermissions(0o600))
	ok, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("acquire meeting source lock: %w", err)
	}
	if !ok {
		_ = lock.Close()
		return nil, errors.New("meeting source lock unavailable")
	}
	return lock, nil
}
