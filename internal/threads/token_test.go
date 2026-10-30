package threads

import (
	"context"
	"fmt"
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
	assert.Error(m.Save(r), "stale generation cannot overwrite a newer record")
	for _, id := range []string{"", "0", "../123", "1/2", "1e3", "-1", "01"} {
		_, err = m.Load(id, oauthTestOwner)
		assert.Error(err)
		bad := testTokenRecord()
		bad.AppID = id
		assert.Error(m.Save(bad))
		assert.Error(m.Delete(id, oauthTestOwner))
	}
	require.NoError(m.Delete("123", oauthTestOwner))
	_, err = m.Load("123", oauthTestOwner)
	assert.ErrorIs(err, ErrTokenNotFound)
	require.NoError(m.Delete("123", oauthTestOwner))
}

func TestThreadsTokenLifecycleRefreshBoundaries(t *testing.T) {
	cases := []struct {
		name             string
		age, expiry      time.Duration
		refresh, invalid bool
	}{
		{"younger than 24h", 24*time.Hour - time.Nanosecond, 6 * 24 * time.Hour, false, false},
		{"exactly 24h near expiry", 24 * time.Hour, 7 * 24 * time.Hour, true, false},
		{"before seven days", 7*24*time.Hour - time.Nanosecond, 30 * 24 * time.Hour, false, false},
		{"exactly seven days", 7 * 24 * time.Hour, 30 * 24 * time.Hour, true, false},
		{"within seven day expiry", 2 * 24 * time.Hour, 7 * 24 * time.Hour, true, false},
		{"outside seven day expiry", 2 * 24 * time.Hour, 7*24*time.Hour + time.Nanosecond, false, false},
		{"expired", 8 * 24 * time.Hour, 0, false, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newOAuthFixture(t)
			m := NewTokenManager(t.TempDir())
			r := testTokenRecord()
			r.IssuedAt = oauthTestNow.Add(-tt.age)
			r.ExpiresAt = oauthTestNow.Add(tt.expiry)
			require.NoError(m.Save(r))
			var got TokenRecord
			err := m.WithSourceLock(context.Background(), oauthTestSource, func() error {
				var err error
				got, err = m.EnsureCurrent(context.Background(), oauthTestSource, f.options())
				return err
			})
			if tt.invalid {
				assert.ErrorIs(err, ErrReauthorizationRequired)
				assert.Empty(got.AccessToken())
				assert.Zero(f.calls.Load())
				return
			}
			require.NoError(err)
			if tt.refresh {
				assert.Equal("refreshed-secret", got.AccessToken())
				assert.Equal(uint64(2), got.Generation)
				assert.Equal(oauthTestNow, got.RefreshedAt)
				assert.Equal([]string{"/refresh_access_token", "/debug_token"}, f.paths)
			} else {
				assert.Equal("long-secret", got.AccessToken())
				assert.Zero(f.calls.Load())
			}
			durable, err := m.Load("123", oauthTestOwner)
			require.NoError(err)
			assert.Equal(got.AccessToken(), durable.AccessToken())
		})
	}
	t.Run("recent refresh resets age", func(t *testing.T) {
		f := newOAuthFixture(t)
		m := NewTokenManager(t.TempDir())
		r := testTokenRecord()
		r.RefreshedAt = oauthTestNow.Add(-time.Hour)
		r.ExpiresAt = oauthTestNow.Add(6 * 24 * time.Hour)
		require.NoError(t, m.Save(r))
		_, err := m.EnsureCurrent(context.Background(), oauthTestSource, f.options())
		require.NoError(t, err)
		assert.Zero(t, f.calls.Load())
	})
	t.Run("data access expiry before refresh", func(t *testing.T) {
		f := newOAuthFixture(t)
		m := NewTokenManager(t.TempDir())
		r := testTokenRecord()
		r.DataAccessExpiresAt = oauthTestNow
		require.NoError(t, m.Save(r))
		_, err := m.EnsureCurrent(context.Background(), oauthTestSource, f.options())
		assert.ErrorIs(t, err, ErrReauthorizationRequired)
		assert.Zero(t, f.calls.Load())
	})
}

func TestThreadsTokenLifecycleInvalidation(t *testing.T) {
	for _, kind := range []string{"graph190", "debug invalid", "debug app", "debug owner", "debug scope", "transient"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newOAuthFixture(t)
			switch kind {
			case "graph190":
				f.failPath = "/refresh_access_token"
				f.failStatus = 400
				f.failCode = 190
			case "debug invalid":
				f.valid = false
			case "debug app":
				f.debugApp = "456"
			case "debug owner":
				f.debugOwner = "789"
			case "debug scope":
				f.scopes = []string{"threads_basic"}
			case "transient":
				f.failPath = "/refresh_access_token"
				f.failStatus = 503
			}
			m := NewTokenManager(t.TempDir())
			require.NoError(m.Save(testTokenRecord()))
			got, err := m.EnsureCurrent(context.Background(), oauthTestSource, f.options())
			require.Error(err)
			assert.Empty(got.AccessToken())
			assert.NotContains(err.Error(), "secret")
			before := f.calls.Load()
			_, err = m.EnsureCurrent(context.Background(), oauthTestSource, f.options())
			require.Error(err)
			if kind == "transient" {
				assert.Greater(f.calls.Load(), before)
				stored, err := m.Load("123", oauthTestOwner)
				require.NoError(err)
				assert.False(stored.Invalidated)
				assert.Equal(uint64(1), stored.Generation)
			} else {
				assert.Equal(before, f.calls.Load(), "disabled generation must not call provider again")
				assert.ErrorIs(err, ErrReauthorizationRequired)
				// A newly verified connection clears only the old generation's invalidation.
				f.failPath = ""
				f.valid = true
				f.debugApp = "123"
				f.debugOwner = oauthTestOwner
				f.scopes = []string{"threads_basic", "threads_read_replies"}
				fresh, err := ExchangeCredential(context.Background(), f.options(), testOAuthRequest())
				require.NoError(err)
				require.NoError(m.Save(fresh))
				stored, err := m.Load("123", oauthTestOwner)
				require.NoError(err)
				assert.Equal(uint64(2), stored.Generation)
				assert.False(stored.Invalidated)
			}
		})
	}
}

func TestThreadsTokenLifecycleSaveFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newOAuthFixture(t)
	dir := t.TempDir()
	m := NewTokenManager(dir)
	require.NoError(m.Save(testTokenRecord()))
	backup := filepath.Join(dir, "old.json")
	f.beforeRefresh = func() {
		require.NoError(os.Rename(tokenFilePath(dir), backup))
		require.NoError(os.Mkdir(tokenFilePath(dir), 0700))
	}
	got, err := m.EnsureCurrent(context.Background(), oauthTestSource, f.options())
	require.Error(err)
	assert.Empty(got.AccessToken(), "unsaved replacement cannot be used")
	require.NoError(os.Remove(tokenFilePath(dir)))
	require.NoError(os.Rename(backup, tokenFilePath(dir)))
	reloaded, err := NewTokenManager(dir).Load("123", oauthTestOwner)
	require.NoError(err)
	assert.Equal("long-secret", reloaded.AccessToken())
	assert.Equal(uint64(1), reloaded.Generation)
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
		assert.Error(m.Save(testTokenRecord()))
		_, err := m.Load("123", oauthTestOwner)
		assert.Error(err)
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
				assert.Error(m.Save(testTokenRecord()))
				_, err := m.Load("123", oauthTestOwner)
				assert.Error(err)
				assert.Error(m.Delete("123", oauthTestOwner))
			} else if name == ".threads-token.lock" {
				assert.Error(m.Save(testTokenRecord()))
			} else {
				assert.Error(m.WithSourceLock(context.Background(), oauthTestSource, func() error { return nil }))
			}
			data, err := os.ReadFile(target)
			require.NoError(err)
			assert.Equal("untouched", string(data))
		})
	}
	t.Run("replaced root during refresh", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newOAuthFixture(t)
		base := t.TempDir()
		dir := filepath.Join(base, "tokens")
		m := NewTokenManager(dir)
		require.NoError(m.Save(testTokenRecord()))
		f.beforeRefresh = func() {
			require.NoError(os.Rename(dir, filepath.Join(base, "old")))
			require.NoError(os.Mkdir(dir, 0700))
		}
		got, err := m.EnsureCurrent(context.Background(), oauthTestSource, f.options())
		require.Error(err)
		assert.Empty(got.AccessToken())
		_, err = os.Stat(tokenFilePath(dir))
		assert.True(os.IsNotExist(err))
	})
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
		err := m.WithSourceLock(ctx, oauthTestSource, func() error { return fmt.Errorf("child entered held lifecycle lock") })
		require.ErrorIs(t, err, context.DeadlineExceeded)
		return
	}
	dir := t.TempDir()
	m := NewTokenManager(dir)
	require.NoError(t, m.WithSourceLock(context.Background(), oauthTestSource, func() error {
		child := exec.Command(os.Args[0], "-test.run=^TestThreadsTokenLifecycleCrossProcessLock$")
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

func TestThreadsTokenLifecycleExplicitInvalidation(t *testing.T) {
	f := newOAuthFixture(t)
	m := NewTokenManager(t.TempDir())
	require.NoError(t, m.WithSourceLock(context.Background(), oauthTestSource, func() error {
		require.NoError(t, m.Save(testTokenRecord()))
		old, err := m.Load("123", oauthTestOwner)
		require.NoError(t, err)
		require.NoError(t, m.Invalidate(old))
		require.NoError(t, m.Invalidate(old), "same generation invalidation is idempotent")
		for range 2 {
			got, err := m.EnsureCurrent(context.Background(), oauthTestSource, f.options())
			assert.ErrorIs(t, err, ErrReauthorizationRequired)
			assert.Empty(t, got.AccessToken())
		}
		assert.Zero(t, f.calls.Load())
		// Model a newly verified reconnect; the newer generation must survive a late
		// Graph 190 response referring to the former credential.
		fresh := testTokenRecord()
		fresh.IssuedAt = oauthTestNow
		require.NoError(t, m.Save(fresh))
		require.Error(t, m.Invalidate(old))
		current, err := m.Load("123", oauthTestOwner)
		require.NoError(t, err)
		assert.Equal(t, uint64(2), current.Generation)
		assert.False(t, current.Invalidated)
		got, err := m.EnsureCurrent(context.Background(), oauthTestSource, f.options())
		require.NoError(t, err)
		assert.Equal(t, uint64(2), got.Generation)
		assert.Zero(t, f.calls.Load())
		require.NoError(t, m.Invalidate(current))
		_, err = m.EnsureCurrent(context.Background(), oauthTestSource, f.options())
		assert.ErrorIs(t, err, ErrReauthorizationRequired)
		assert.Zero(t, f.calls.Load())
		return nil
	}))
	assert.Error(t, m.Invalidate(TokenRecord{}))
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

// The generated app=1/owner=1 case pins stale invalidation's error contract.
func TestThreadsCredentialNamespaceStaleInvalidationRegression(t *testing.T) {
	manager := NewTokenManager(t.TempDir())
	record := testTokenRecord()
	record.AppID, record.OwnerID = "1", "1"
	require.NoError(t, manager.WithSourceLock(t.Context(), "threads:1:1", func() error {
		require.NoError(t, manager.Save(record))
		saved, err := manager.Load("1", "1")
		require.NoError(t, err)
		require.NoError(t, manager.Save(record))
		require.Error(t, manager.Invalidate(saved))
		latest, err := manager.Load("1", "1")
		require.NoError(t, err)
		assert.Equal(t, uint64(2), latest.Generation)
		assert.False(t, latest.Invalidated)
		return nil
	}))
}
