package providercredentials

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Removing source precedence or fail-closed reads would expose a lower-priority key.
func TestResolveSecretPrecedenceAndFailures(t *testing.T) { //nolint:paralleltest // secret environment is process-wide
	t.Setenv("MSGVAULT_TEST_SECRET", "environment-secret")
	file := filepath.Join(t.TempDir(), "secret")
	prepareSecretFixture(t, file, []byte(" file-secret\n"))
	missing := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		name, inline, file, env, want string
		wantError                     bool
	}{
		{"inline", "inline-secret", missing, "MISSING_SECRET_ENV", "inline-secret", false},
		{"file", "", file, "MSGVAULT_TEST_SECRET", "file-secret", false},
		{"env", "", "", "MSGVAULT_TEST_SECRET", "environment-secret", false},
		{"none", "", "", "", "", false},
		{"missing file", "", missing, "MSGVAULT_TEST_SECRET", "", true},
		{"missing env", "", "", "MISSING_SECRET_ENV", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			got, err := ResolveSecret(tc.inline, tc.file, tc.env)
			if tc.wantError {
				require.Error(err)
				assert.Empty(got)
				assert.NotContains(err.Error(), "environment-secret")
				return
			}
			require.NoError(err)
			assert.Equal(tc.want, got)
		})
	}
}

func prepareSecretFixture(t *testing.T, path string, contents []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	handle, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = handle.Close() })
	require.NoError(t, (nativePermissions{}).secureFile(handle))
	require.NoError(t, handle.Close())
}

func TestReadSecretFileRejectsEmptyAndOversized(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	for _, value := range []string{" \r\n", strings.Repeat("x", 65537)} {
		path := filepath.Join(t.TempDir(), "secret")
		require.NoError(os.WriteFile(path, []byte(value), 0o600))
		file, err := os.Open(path)
		require.NoError(err)
		require.NoError((nativePermissions{}).secureFile(file))
		require.NoError(file.Close())
		got, err := ReadSecretFile(path)
		require.Error(err)
		assert.Empty(t, got)
	}
}

func TestReadSecretFileAcceptsReadOnlyMountedInput(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("Unix read-only mode; native ACL reads covered by common secret tests")
	}
	parent := t.TempDir()
	require.NoError(os.Chmod(parent, 0o755))
	path := filepath.Join(parent, "secret")
	require.NoError(os.WriteFile(path, []byte("mounted-secret\n"), 0o400))
	got, err := ReadSecretFile(path)
	require.NoError(err)
	assert.Equal("mounted-secret", got)
	info, err := os.Stat(path)
	require.NoError(err)
	assert.Equal(os.FileMode(0o400), info.Mode().Perm())
	require.NoError(os.Chmod(path, 0o644))
	_, err = ReadSecretFile(path)
	require.Error(err)
	link := filepath.Join(parent, "link")
	require.NoError(os.Symlink(path, link))
	_, err = ReadSecretFile(link)
	require.Error(err)
}

// Publishing without a lock or replacing a file would rotate a key on concurrent starts.
func TestEnsureServerKeyPersistsAcrossConcurrentStarts(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	tokens := filepath.Join(t.TempDir(), "tokens")
	type result struct {
		key string
		err error
	}
	results := make(chan result, 12)
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			key, err := EnsureServerKey(tokens)
			results <- result{key, err}
		})
	}
	workers.Wait()
	close(results)
	var want string
	for got := range results {
		require.NoError(got.err)
		assert.Len(got.key, 43)
		if want == "" {
			want = got.key
		}
		assert.Equal(want, got.key)
	}
	persisted, err := ReadSecretFile(filepath.Join(tokens, "server-api-key"))
	require.NoError(err)
	assert.Equal(want, persisted)
	restarted, err := EnsureServerKey(tokens)
	require.NoError(err)
	assert.Equal(want, restarted)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(tokens, "server-api-key"))
		require.NoError(err)
		assert.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestEnsureServerKeyDoesNotReplaceInvalidExistingFile(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	tokens := t.TempDir()
	path := filepath.Join(tokens, "server-api-key")
	require.NoError(os.WriteFile(path, nil, 0o600))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	require.NoError((nativePermissions{}).secureFile(file))
	require.NoError(file.Close())
	_, err = EnsureServerKey(tokens)
	require.Error(err)
	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.Empty(data)
}
