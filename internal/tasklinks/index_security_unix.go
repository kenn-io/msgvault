//go:build !windows

package tasklinks

import (
	"fmt"
	"os"
	"syscall"
)

func cachePersistencePolicy() error {
	return nil
}

func secureCacheDirectory(path string) error {
	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // directories require execute permission
		return fmt.Errorf("secure task cache directory: %w", err)
	}
	return nil
}

func secureCacheFile(file *os.File) error {
	return file.Chmod(0o600)
}

func validateCacheFile(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("task reverse index %q must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("task reverse index %q is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("task reverse index %q must have mode 0600", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) { //nolint:gosec // POSIX UIDs are non-negative and stored as uint32
		return fmt.Errorf("task reverse index %q is not owned by the current user", path)
	}
	return nil
}
