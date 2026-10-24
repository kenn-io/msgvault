//go:build linux

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type handleCall struct {
	dirfd int
	path  string
	flags int
}

// stubLinuxIdentitySyscalls replaces statx with one that reports a fixed
// device and inode without birth time, and name_to_handle_at with handle.
func stubLinuxIdentitySyscalls(
	t *testing.T,
	handle func(dirfd int, path string, flags int) (unix.FileHandle, int, error),
) {
	t.Helper()
	statx, nameToHandleAt := linuxStatx, linuxNameToHandleAt
	t.Cleanup(func() { linuxStatx, linuxNameToHandleAt = statx, nameToHandleAt })
	linuxStatx = func(_ int, _ string, _ int, _ int, stat *unix.Statx_t) error {
		*stat = unix.Statx_t{Mask: unix.STATX_INO, Dev_major: 8, Dev_minor: 1, Ino: 42}
		return nil
	}
	linuxNameToHandleAt = handle
}

func TestLinuxFileIdentityWithoutBirthTimeUsesFileHandle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var generation byte = 1
	var calls []handleCall
	stubLinuxIdentitySyscalls(t, func(dirfd int, path string, flags int) (unix.FileHandle, int, error) {
		calls = append(calls, handleCall{dirfd: dirfd, path: path, flags: flags})
		// Same inode number, distinguished only by its generation.
		return unix.NewFileHandle(1, []byte{42, 0, 0, 0, generation, 0, 0, 0}), 0, nil
	})

	first, ok := linuxFileIdentity(unix.AT_FDCWD, "config.toml", 0)
	require.True(ok)
	again, ok := linuxFileIdentity(unix.AT_FDCWD, "config.toml", 0)
	require.True(ok)
	assert.Equal(first, again)

	generation = 2
	reused, ok := linuxFileIdentity(unix.AT_FDCWD, "config.toml", 0)
	require.True(ok)
	assert.NotEqual(first, reused)
	assert.Len(calls, 3)
}

func TestLinuxFileIdentityWithoutBirthTimeHandleErrorPolicy(t *testing.T) {
	var handleErr error
	stubLinuxIdentitySyscalls(t, func(int, string, int) (unix.FileHandle, int, error) {
		if handleErr != nil {
			return unix.FileHandle{}, 0, handleErr
		}
		return unix.NewFileHandle(1, []byte{42, 0, 0, 0, 1, 0, 0, 0}), 0, nil
	})
	withHandle, ok := linuxFileIdentity(unix.AT_FDCWD, "config.toml", 0)
	require.True(t, ok)

	var deviceAndInode string
	for _, errno := range []unix.Errno{unix.EOPNOTSUPP, unix.ENOSYS, unix.EPERM} {
		t.Run("falls back on "+errno.Error(), func(t *testing.T) {
			handleErr = errno
			identity, ok := linuxFileIdentity(unix.AT_FDCWD, "config.toml", 0)
			require.True(t, ok)
			assert.NotEqual(t, withHandle, identity)
			if deviceAndInode == "" {
				deviceAndInode = identity
			}
			assert.Equal(t, deviceAndInode, identity)
		})
	}
	for _, errno := range []unix.Errno{unix.EACCES, unix.ENOENT, unix.ELOOP, unix.EINVAL} {
		t.Run("fails closed on "+errno.Error(), func(t *testing.T) {
			handleErr = errno
			identity, ok := linuxFileIdentity(unix.AT_FDCWD, "config.toml", 0)
			assert.False(t, ok)
			assert.Empty(t, identity)
		})
	}
}

func TestLinuxFileIdentityWithBirthTimeSkipsFileHandle(t *testing.T) {
	statx, nameToHandleAt := linuxStatx, linuxNameToHandleAt
	t.Cleanup(func() { linuxStatx, linuxNameToHandleAt = statx, nameToHandleAt })
	linuxStatx = func(_ int, _ string, _ int, _ int, stat *unix.Statx_t) error {
		*stat = unix.Statx_t{
			Mask:      unix.STATX_INO | unix.STATX_BTIME,
			Dev_major: 8, Dev_minor: 1, Ino: 42,
			Btime: unix.StatxTimestamp{Sec: 1700000000, Nsec: 5},
		}
		return nil
	}
	linuxNameToHandleAt = func(int, string, int) (unix.FileHandle, int, error) {
		assert.Fail(t, "file handle requested although birth time is available")
		return unix.FileHandle{}, 0, unix.EOPNOTSUPP
	}

	identity, ok := linuxFileIdentity(unix.AT_FDCWD, "config.toml", 0)
	require.True(t, ok)
	assert.Equal(t, "linux:8:1:42:1700000000:5", identity)
}

// withoutBirthTime makes identity lookups on the real filesystem behave as on
// one that does not report birth time, such as ext4 with 128-byte inodes.
// Test runners rarely have such a filesystem mounted, so the statx result is
// masked instead. With fileHandles, the test is skipped unless the
// filesystem under t.TempDir can encode them; otherwise handle lookup reports
// EOPNOTSUPP and identity falls back to device and inode.
func withoutBirthTime(t *testing.T, fileHandles bool) {
	t.Helper()
	statx, nameToHandleAt := linuxStatx, linuxNameToHandleAt
	t.Cleanup(func() { linuxStatx, linuxNameToHandleAt = statx, nameToHandleAt })
	if fileHandles {
		if _, _, err := nameToHandleAt(unix.AT_FDCWD, t.TempDir(), 0); err != nil {
			t.Skipf("temporary directory filesystem cannot encode file handles: %v", err)
		}
	}
	linuxStatx = func(dirfd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		err := statx(dirfd, path, flags, mask&^unix.STATX_BTIME, stat)
		stat.Mask &^= unix.STATX_BTIME
		return err
	}
	linuxNameToHandleAt = func(dirfd int, path string, flags int) (unix.FileHandle, int, error) {
		if !fileHandles {
			return unix.FileHandle{}, 0, unix.EOPNOTSUPP
		}
		handle, mountID, err := nameToHandleAt(dirfd, path, flags)
		assert.NoError(t, err, "file handle lookup must succeed in file-handle mode")
		return handle, mountID, err
	}
}

var noBirthTimeModes = []struct {
	name        string
	fileHandles bool
}{
	{name: "file handles", fileHandles: true},
	{name: "device and inode", fileHandles: false},
}

func TestConfigIdentityWithoutBirthTime(t *testing.T) {
	for _, mode := range noBirthTimeModes {
		t.Run(mode.name, func(t *testing.T) {
			withoutBirthTime(t, mode.fileHandles)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))

			first := mustConfigIdentity(t, path)
			require.NotEmpty(first)
			assert.Equal(t, first, mustConfigIdentity(t, path))

			// Hold the replaced file open so its inode number cannot be reused.
			replaced, err := os.Open(path)
			require.NoError(err)
			t.Cleanup(func() { _ = replaced.Close() })
			require.NoError(os.Remove(path))
			require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
			assert.NotEqual(t, first, mustConfigIdentity(t, path))
		})
	}
}

func TestLinuxFileIdentityWithoutBirthTimeDoesNotFollowFinalSymlink(t *testing.T) {
	for _, mode := range noBirthTimeModes {
		t.Run(mode.name, func(t *testing.T) {
			withoutBirthTime(t, mode.fileHandles)
			require := require.New(t)
			dir := t.TempDir()
			target := filepath.Join(dir, "config.toml")
			require.NoError(os.WriteFile(target, []byte("[web]\ntheme = \"system\"\n"), 0o600))
			link := filepath.Join(dir, "link.toml")
			require.NoError(os.Symlink(target, link))

			targetIdentity, ok := pathEntryIdentity(target, nil)
			require.True(ok)
			linkIdentity, ok := pathEntryIdentity(link, nil)
			require.True(ok)
			assert.NotEqual(t, targetIdentity, linkIdentity)

			// An opened descriptor names the same object as its path. The
			// resolver opens symlinks with O_PATH|O_NOFOLLOW, so cover that too.
			file, err := os.Open(target)
			require.NoError(err)
			t.Cleanup(func() { _ = file.Close() })
			openedTarget, ok := openedUnixFileIdentity(file, nil)
			require.True(ok)
			assert.Equal(t, targetIdentity, openedTarget)

			fd, err := unix.Open(link, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			require.NoError(err)
			linkFile := os.NewFile(uintptr(fd), link)
			t.Cleanup(func() { _ = linkFile.Close() })
			openedLink, ok := openedUnixFileIdentity(linkFile, nil)
			require.True(ok)
			assert.Equal(t, linkIdentity, openedLink)
		})
	}
}

func TestEditConfigWithoutBirthTime(t *testing.T) {
	for _, mode := range noBirthTimeModes {
		t.Run(mode.name+"/save", func(t *testing.T) {
			withoutBirthTime(t, mode.fileHandles)
			require := require.New(t)
			cfg := NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Server.APIPort = 9090

			require.NoError(cfg.Save())
			loaded, err := Load(cfg.ConfigFilePath(), "")
			require.NoError(err)
			assert.Equal(t, 9090, loaded.Server.APIPort)
		})
		t.Run(mode.name+"/existing file", func(t *testing.T) {
			withoutBirthTime(t, mode.fileHandles)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))

			before, err := ReadConfigFile(path)
			require.NoError(err)
			after, err := EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
			require.NoError(err)
			assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(after.Content))
		})
		t.Run(mode.name+"/missing file", func(t *testing.T) {
			withoutBirthTime(t, mode.fileHandles)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "settings", "config.toml")

			before, err := ReadConfigFile(path)
			require.NoError(err)
			require.False(before.Exists)
			after, err := EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
			require.NoError(err)
			assert.Contains(t, string(after.Content), `theme = "dark"`)
		})
		t.Run(mode.name+"/same-content substitution", func(t *testing.T) {
			withoutBirthTime(t, mode.fileHandles)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			beforeText := "[web]\ntheme = \"system\"\n"
			require.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
			before, err := ReadConfigFile(path)
			require.NoError(err)
			ops := defaultConfigFileOps()
			ops.beforeExchange = func() error {
				if err := os.Remove(path); err != nil {
					return err
				}
				return os.WriteFile(path, []byte(beforeText), 0o600)
			}

			_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
			require.ErrorIs(err, ErrConfigConflict)
			assert.Equal(t, beforeText, string(mustReadFile(t, path)))
		})
		t.Run(mode.name+"/parent directory swap", func(t *testing.T) {
			withoutBirthTime(t, mode.fileHandles)
			require := require.New(t)
			root := t.TempDir()
			parent := filepath.Join(root, "active")
			require.NoError(os.Mkdir(parent, 0o700))
			path := filepath.Join(parent, "config.toml")
			before, err := ReadConfigFile(path)
			require.NoError(err)
			ops := defaultConfigFileOps()
			ops.beforeExchange = func() error {
				if err := os.Rename(parent, filepath.Join(root, "original")); err != nil {
					return err
				}
				return os.Mkdir(parent, 0o700)
			}

			_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
			require.ErrorIs(err, ErrConfigConflict)
			assert.NoFileExists(t, path)
		})
	}
}
