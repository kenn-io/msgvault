package threads

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const oauthTestSource = "threads:123:9007199254740993123"

func testTokenRecord() TokenRecord {
	return TokenRecord{AppID: "123", OwnerID: oauthTestOwner, Username: "example_owner", Name: "Example Owner", Scopes: []string{"threads_basic", "threads_read_replies"}, IssuedAt: oauthTestNow.Add(-8 * 24 * time.Hour), ExpiresAt: oauthTestNow.Add(30 * 24 * time.Hour), DataAccessExpiresAt: oauthTestNow.Add(90 * 24 * time.Hour), RedirectURI: oauthTestCallback, secret: &credentialSecret{value: "long-secret"}}
}
func tokenFilePath(dir string) string {
	return filepath.Join(dir, "threads_123_"+oauthTestOwner+".json")
}

func TestThreadsTokenLifecycleStorage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	m := NewTokenManager(dir)
	_, err := m.Load("123", oauthTestOwner)
	require.ErrorIs(err, ErrTokenNotFound)
	require.NoError(m.Save(testTokenRecord()))
	r, err := m.Load("123", oauthTestOwner)
	require.NoError(err)
	assert.Equal("long-secret", r.AccessToken())
	assert.Equal(uint64(1), r.Generation)
	info, err := os.Stat(tokenFilePath(dir))
	require.NoError(err)
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0600), info.Mode().Perm())
	}
	bytes, err := os.ReadFile(tokenFilePath(dir))
	require.NoError(err)
	assert.NotContains(string(bytes), "app-secret")
	assert.NotContains(string(bytes), "authorization-code")
	r.secret = &credentialSecret{value: "new-secret"}
	require.NoError(m.Save(r))
	next, err := m.Load("123", oauthTestOwner)
	require.NoError(err)
	assert.Equal(uint64(2), next.Generation)
	assert.Equal("new-secret", next.AccessToken())
	require.Error(m.Save(r), "stale generation cannot overwrite a newer record")
	for _, id := range []string{"", "0", "../123", "1/2", "1e3", "-1", "01"} {
		_, err = m.Load(id, oauthTestOwner)
		require.Error(err)
		bad := testTokenRecord()
		bad.AppID = id
		require.Error(m.Save(bad))
		require.Error(m.Delete(id, oauthTestOwner))
	}
	require.NoError(m.Delete("123", oauthTestOwner))
	_, err = m.Load("123", oauthTestOwner)
	require.ErrorIs(err, ErrTokenNotFound)
	require.NoError(m.Delete("123", oauthTestOwner))
}

func TestThreadsTokenLifecycleFilesystem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires Unix symlink privileges")
	}
	t.Run("root symlink", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		base := t.TempDir()
		target := filepath.Join(base, "target")
		require.NoError(os.Mkdir(target, 0700))
		link := filepath.Join(base, "link")
		require.NoError(os.Symlink(target, link))
		m := NewTokenManager(link)
		require.Error(m.Save(testTokenRecord()))
		_, err := m.Load("123", oauthTestOwner)
		require.Error(err)
		assert.Error(m.WithSourceLock(context.Background(), oauthTestSource, func() error { return nil }))
	})
	for _, name := range []string{"threads_123_" + oauthTestOwner + ".json", ".threads-token.lock", ".threads_123_" + oauthTestOwner + ".lock"} {
		t.Run("symlink "+name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			target := filepath.Join(t.TempDir(), "target")
			require.NoError(os.WriteFile(target, []byte("untouched"), 0600))
			require.NoError(os.Symlink(target, filepath.Join(dir, name)))
			m := NewTokenManager(dir)
			if strings.HasSuffix(name, ".json") {
				require.Error(m.Save(testTokenRecord()))
				_, err := m.Load("123", oauthTestOwner)
				require.Error(err)
				require.Error(m.Delete("123", oauthTestOwner))
			} else if name == ".threads-token.lock" {
				require.Error(m.Save(testTokenRecord()))
			} else {
				require.Error(m.WithSourceLock(context.Background(), oauthTestSource, func() error {
					return nil
				}))
			}
			data, err := os.ReadFile(target)
			require.NoError(err)
			assert.Equal("untouched", string(data))
		})
	}
	t.Run("malformed file hides content", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(tokenFilePath(dir), []byte(`{"access_token":"file-secret",`), 0600))
		_, err := NewTokenManager(dir).Load("123", oauthTestOwner)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "file-secret")
	})
}

func TestThreadsTokenLifecycleCrossProcessLock(t *testing.T) {
	// The subprocess proves OS-level coordination rather than just a shared mutex.
	if dir := os.Getenv("MSGVAULT_THREADS_LOCK_TEST_DIR"); dir != "" {
		// The lock must stay held until the child context expires.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		m := NewTokenManager(dir)
		err := m.WithSourceLock(ctx, oauthTestSource, func() error { return errors.New("child entered held lifecycle lock") })
		require.ErrorIs(t, err, context.DeadlineExceeded)
		return
	}
	dir := t.TempDir()
	m := NewTokenManager(dir)
	require.NoError(t, m.WithSourceLock(context.Background(), oauthTestSource, func() error {
		binaryPath, err := os.Executable()
		require.NoError(t, err)
		child := exec.CommandContext(t.Context(), binaryPath, "-test.run=^TestThreadsTokenLifecycleCrossProcessLock$")
		child.Env = append(os.Environ(), "MSGVAULT_THREADS_LOCK_TEST_DIR="+dir)
		output, err := child.CombinedOutput()
		require.NoError(t, err, string(output))
		// Independent flock instance must observe the held source lock.
		held := flock.New(filepath.Join(dir, ".threads_123_"+oauthTestOwner+".lock"))
		ok, err := held.TryLock()
		require.NoError(t, err)
		assert.False(t, ok)
		if ok {
			require.NoError(t, held.Unlock())
		}
		// Saving takes a different lock and must complete inside lifecycle ownership.
		return m.Save(testTokenRecord())
	}))
	for _, source := range []string{"threads:../:1", "threads:123:1:2", "123", "x:123:1"} {
		assert.Error(t, m.WithSourceLock(context.Background(), source, func() error { return nil }))
	}
}

func TestThreadsLongCredentialNamespaceRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	manager := NewTokenManager(t.TempDir())
	record := testTokenRecord()
	record.AppID = strings.Repeat("1", 128)
	record.OwnerID = strings.Repeat("2", 128)
	source, err := SourceIdentifier(record.AppID, record.OwnerID)
	require.NoError(err)
	require.NoError(manager.WithSourceLock(t.Context(), source, func() error { return manager.Save(record) }))
	saved, err := manager.Load(record.AppID, record.OwnerID)
	require.NoError(err)
	assert.Equal(record.AppID, saved.AppID)
	assert.Equal(record.OwnerID, saved.OwnerID)
	assert.Equal(record.AccessToken(), saved.AccessToken())
	require.NoError(manager.Delete(record.AppID, record.OwnerID))
	_, err = manager.Load(record.AppID, record.OwnerID)
	require.ErrorIs(err, ErrTokenNotFound)
}
