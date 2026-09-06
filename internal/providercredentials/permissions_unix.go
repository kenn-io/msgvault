//go:build !windows

package providercredentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type nativePermissions struct{}

func (nativePermissions) secureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("provider credential directory must be a real directory")
	}
	if err := os.Chmod(path, 0o700); err != nil { // #nosec G302 -- exact private directory mode.
		return err
	}
	return nativePermissions{}.verifyDirectory(path)
}

func (nativePermissions) verifyDirectory(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open provider credential directory: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close() //nolint:errcheck // read-only directory descriptor
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("provider credential directory permissions must be 0700")
	}
	return verifyCurrentOwner(info)
}

func (nativePermissions) secureFile(file *os.File) error {
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return nativePermissions{}.verifyFile(file)
}

func (nativePermissions) verifyFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("provider credential file permissions must be 0600")
	}
	return verifyCurrentOwner(info)
}

func verifyCurrentOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("provider credential owner is unavailable")
	}
	if stat.Uid != uint32(os.Geteuid()) { //nolint:gosec // OS effective UIDs are non-negative and fit the platform uid_t.
		return errors.New("provider credential object is not owned by the current user")
	}
	return nil
}

func openStoreFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open provider credential store: %w", err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

func withStoreLock(tokenDir string, fn func() error) error {
	path := filepath.Join(tokenDir, ".provider-credentials.lock")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("open provider credential lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close() //nolint:errcheck // lock result is returned by fn
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure provider credential lock: %w", err)
	}
	if err := (nativePermissions{}).verifyFile(file); err != nil {
		return fmt.Errorf("verify provider credential lock: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock provider credential store: %w", err)
	}
	defer unix.Flock(fd, unix.LOCK_UN) //nolint:errcheck // closing also releases the lock
	return fn()
}

func replaceStoreFile(source, target string) error {
	return os.Rename(source, target)
}

func syncStoreDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck // Sync result is authoritative
	return directory.Sync()
}
