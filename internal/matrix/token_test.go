package matrix

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
)

func TestCredentialLifecycleLockSerializesCallers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- WithCredentialLifecycleLock(dir, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		require.FailNow("first lifecycle operation did not acquire lock")
	}

	var secondEntered atomic.Bool
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- WithCredentialLifecycleLock(dir, func() error {
			secondEntered.Store(true)
			return nil
		})
	}()
	select {
	case err := <-secondDone:
		require.FailNow("second lifecycle operation bypassed lock", "error: %v", err)
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // absence check: the first operation holds the lifecycle lock
		assert.False(secondEntered.Load())
	}
	close(releaseFirst)
	require.NoError(<-firstDone)
	require.NoError(<-secondDone)
	assert.True(secondEntered.Load())
}

func TestCredentialsRoundTripUsesPrivateFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	want := Credentials{Homeserver: "https://matrix.example.org", UserID: "@archive:example.org", DeviceID: "DEVICE1", AccessToken: "secret"}
	require.NoError(SaveCredentials(dir, want))
	got, err := LoadCredentials(dir, want.UserID)
	require.NoError(err)
	assert.Equal(want, got)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(tokenPath(dir, want.UserID))
		require.NoError(err)
		assert.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
	require.NoError(DeleteCredentials(dir, want.UserID))
	_, err = os.Stat(tokenPath(dir, want.UserID))
	require.ErrorIs(err, os.ErrNotExist)
}

func TestSaveCredentialsKeepsPublishedFileAfterReplaceFailure(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	creds := Credentials{Homeserver: "https://matrix.example.org", UserID: "@archive:example.org", DeviceID: "DEVICE1", AccessToken: "secret"}
	original := secureReplaceCredentials
	t.Cleanup(func() { secureReplaceCredentials = original })
	secureReplaceCredentials = func(path string, data []byte, mode os.FileMode) error {
		require.NoError(os.WriteFile(path, data, mode))
		return fmt.Errorf("synthetic directory sync failure: %w", atomicfile.ErrPublished)
	}

	err := SaveCredentials(dir, creds)
	require.ErrorIs(err, atomicfile.ErrPublished)
	got, err := LoadCredentials(dir, creds.UserID)
	require.NoError(err, "the published login is the only one left, so it stays")
	require.Equal(creds, got)
}

func TestSaveCredentialsKeepsExistingFileAfterUnpublishedReplaceFailure(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	creds := Credentials{Homeserver: "https://matrix.example.org", UserID: "@archive:example.org", DeviceID: "DEVICE1", AccessToken: "secret"}
	path := tokenPath(dir, creds.UserID)
	require.NoError(os.WriteFile(path, []byte("existing"), 0o600))
	original := secureReplaceCredentials
	t.Cleanup(func() { secureReplaceCredentials = original })
	secureReplaceCredentials = func(string, []byte, os.FileMode) error {
		return errors.New("synthetic staging failure")
	}

	err := SaveCredentials(dir, creds)
	require.ErrorContains(err, "synthetic staging failure")
	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(t, "existing", string(data))
}
