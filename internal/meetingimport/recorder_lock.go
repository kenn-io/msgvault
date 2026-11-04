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
	canonical, err = filepath.Abs(dbPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve meeting root: %w", err)
	}
	canonical, err = filepath.EvalSymlinks(canonical)
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
