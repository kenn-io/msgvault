package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

const writeOwnerLockFile = "db.write.lock"

type writeOwnerLock struct {
	path string
	lock *flock.Flock
}

func writeOwnerLockPath(dataDir string) string {
	return filepath.Join(dataDir, writeOwnerLockFile)
}

func acquireWriteOwnerLock(ctx context.Context, dataDir string) (*writeOwnerLock, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return tryAcquireWriteOwnerLock(dataDir)
}

func tryAcquireWriteOwnerLock(dataDir string) (*writeOwnerLock, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir for write lock: %w", err)
	}

	path := writeOwnerLockPath(dataDir)
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire sqlite write-owner lock %s: %w", path, err)
	}
	if !locked {
		return nil, writeOwnerLockHeldError{path: path}
	}
	return &writeOwnerLock{path: path, lock: lock}, nil
}

func (l *writeOwnerLock) Close() error {
	if l == nil || l.lock == nil {
		return nil
	}
	if err := l.lock.Unlock(); err != nil {
		return fmt.Errorf("release sqlite write-owner lock %s: %w", l.path, err)
	}
	return nil
}

type writeOwnerLockHeldError struct {
	path string
}

func (e writeOwnerLockHeldError) Error() string {
	return fmt.Sprintf(
		"sqlite archive is already owned by another msgvault process "+
			"(write-owner lock %s is held); stop 'msgvault serve', "+
			"wait for the operation to finish, or retry later",
		e.path,
	)
}
