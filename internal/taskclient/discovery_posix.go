//go:build !windows

package taskclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func currentUserID() uint32 {
	// Unix effective user IDs are represented by the kernel as unsigned
	// 32-bit values even though os.Geteuid exposes an int.
	return uint32(os.Geteuid()) // #nosec G115 -- kernel UID width is uint32
}

func descriptorFileSecurityCheck() error { return nil }

func fileOwnerID(path string) (uint32, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	return fileInfoOwnerID(info)
}

func fileInfoOwnerID(info os.FileInfo) (uint32, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("file owner unavailable")
	}
	return stat.Uid, nil
}

func openSecureRegularFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open secure file without following links: %w", err)
	}
	return os.NewFile(uintptr(fd), filepath.Base(path)), nil
}

func validateSecureSocket(path string, expectedOwner uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: Unix socket is missing", ErrUnreachable)
		}
		return fmt.Errorf("%w: inspect Unix socket", ErrInsecureEndpoint)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: Unix socket must be non-symlinked and private", ErrInsecureEndpoint)
	}
	owner, err := fileInfoOwnerID(info)
	if err != nil || owner != expectedOwner {
		return fmt.Errorf("%w: Unix socket owner mismatch", ErrInsecureEndpoint)
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || parent.Mode()&os.ModeSymlink != 0 || !parent.IsDir() || parent.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: Unix socket parent must be a private non-symlinked directory", ErrInsecureEndpoint)
	}
	parentOwner, err := fileInfoOwnerID(parent)
	if err != nil || parentOwner != expectedOwner {
		return fmt.Errorf("%w: Unix socket parent owner mismatch", ErrInsecureEndpoint)
	}
	return nil
}
