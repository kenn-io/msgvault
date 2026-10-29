package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionStoreRoundTripAndRedaction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := secureTempDir(t)
	store := NewSessionStore(dir, 123, "hash-secret", 456)
	require.NoError(store.StoreSession(context.Background(), []byte("session-secret")))
	data, err := store.LoadSession(context.Background())
	require.NoError(err)
	assert.Equal([]byte("session-secret"), data)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		got := fmt.Sprintf(verb, store)
		assert.NotContains(got, "hash-secret")
		assert.NotContains(got, "session-secret")
	}
	info, err := os.Stat(filepath.Join(dir, "telegram_456.json"))
	require.NoError(err)
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0600), info.Mode().Perm())
	}
}

func TestSessionStoreRejectsSymlinkRootAndLock(t *testing.T) {
	require := require.New(t)

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges")
	}
	dir := secureTempDir(t)
	root := filepath.Join(dir, "link")
	require.NoError(os.Symlink(t.TempDir(), root))
	err := NewSessionStore(root, 123, "hash", 456).StoreSession(context.Background(), []byte("data"))
	require.Error(err)
	require.NoError(os.Symlink(filepath.Join(dir, "other"), filepath.Join(dir, ".telegram-token.lock")))
	err = NewSessionStore(dir, 123, "hash", 456).StoreSession(context.Background(), []byte("data"))
	require.Error(err)
}

func TestSessionStoreReopen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := secureTempDir(t)
	original := NewSessionStore(dir, 123, "hash-secret", 456)
	require.NoError(original.StoreSession(context.Background(), []byte("session-secret")))
	reopened, err := OpenSessionStore(dir, 456)
	require.NoError(err)
	appID, appHash := reopened.ApplicationCredentials()
	assert.Equal(123, appID)
	assert.Equal("hash-secret", appHash)
	data, err := reopened.LoadSession(context.Background())
	require.NoError(err)
	assert.Equal([]byte("session-secret"), data)
}

func TestLifecycleLockIsHeldDuringOperation(t *testing.T) {
	dir := secureTempDir(t)
	store := NewSessionStore(dir, 123, "hash", 456)
	err := store.WithLifecycleLock(func() error {
		competing := flock.New(filepath.Join(dir, ".telegram-lifecycle.lock"))
		locked, err := competing.TryLock()
		require.NoError(t, err)
		if locked {
			require.NoError(t, competing.Unlock())
		}
		assert.False(t, locked)
		return nil
	})
	require.NoError(t, err)
}

func TestSessionStoreRejectsSymlinkFile(t *testing.T) {
	require := require.New(t)

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges")
	}
	dir := secureTempDir(t)
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(os.WriteFile(target, []byte("untouched"), 0600))
	require.NoError(os.Symlink(target, filepath.Join(dir, "telegram_456.json")))
	store := NewSessionStore(dir, 123, "hash", 456)
	_, err := store.LoadSession(context.Background())
	require.Error(err)
	require.Error(store.StoreSession(context.Background(), []byte("data")))
	data, err := os.ReadFile(target)
	require.NoError(err)
	assert.Equal(t, "untouched", string(data))
}

func TestSessionStoreRejectsPermissivePaths(t *testing.T) {
	require := require.New(t)

	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	dir := secureTempDir(t)
	store := NewSessionStore(dir, 123, "hash", 456)
	require.NoError(os.Chmod(dir, 0755))
	require.Error(store.StoreSession(context.Background(), []byte("secret")))
	require.NoError(os.Chmod(dir, 0700))
	require.NoError(store.StoreSession(context.Background(), []byte("secret")))
	require.NoError(os.Chmod(filepath.Join(dir, "telegram_456.json"), 0644))
	_, err := store.LoadSession(context.Background())
	require.Error(err)
}

func secureTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	return dir
}

func TestStoreSessionCancellationWhileLocked(t *testing.T) {
	dir := secureTempDir(t)
	lock := flock.New(filepath.Join(dir, ".telegram-token.lock"))
	require.NoError(t, lock.Lock())
	defer func() { _ = lock.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- NewSessionStore(dir, 123, "hash", 456).StoreSession(ctx, []byte("secret")) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(3 * time.Second):
		assert.Fail(t, "session write did not respond to context cancellation")
	}
}

func TestSessionStoreRejectsPermissiveLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	dir := secureTempDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".telegram-token.lock"), nil, 0600))
	require.NoError(t, os.Chmod(filepath.Join(dir, ".telegram-token.lock"), 0666))
	store := NewSessionStore(dir, 123, "hash", 456)
	require.Error(t, store.StoreSession(context.Background(), []byte("secret")))
}

func TestLifecycleLockCancellation(t *testing.T) {
	dir := secureTempDir(t)
	lock := flock.New(filepath.Join(dir, ".telegram-lifecycle.lock"))
	require.NoError(t, lock.Lock())
	defer func() { _ = lock.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- NewSessionStore(dir, 123, "hash", 456).WithLifecycleLockContext(ctx, func() error { return nil })
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(3 * time.Second):
		assert.Fail(t, "lifecycle lock did not respond to cancellation")
	}
}

func TestDeleteSessionRejectsSymlink(t *testing.T) {
	require := require.New(t)

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges")
	}
	dir := secureTempDir(t)
	target := filepath.Join(dir, "other")
	require.NoError(os.WriteFile(target, []byte("synthetic"), 0600))
	require.NoError(os.Symlink(target, filepath.Join(dir, "telegram_456.json")))
	err := WithLifecycleLockContext(context.Background(), dir, func() error { return DeleteSession(context.Background(), dir, 456) })
	require.Error(err)
	data, err := os.ReadFile(target)
	require.NoError(err)
	assert.Equal(t, "synthetic", string(data))
}
