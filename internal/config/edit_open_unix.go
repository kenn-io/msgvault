//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package config

import (
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func openConfigNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open config without following final symlink: %w", err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openedFileIdentity(file *os.File, info fs.FileInfo) (string, bool) {
	return openedUnixFileIdentity(file, info)
}
