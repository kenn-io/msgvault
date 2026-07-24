//go:build darwin && !cgo

package config

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func openPinnedPathEntry(parent *os.File, name, displayPath string) (*os.File, fs.FileInfo, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == unix.ELOOP {
		return nil, nil, errors.Join(ErrUnsafeConfigTarget,
			errors.New("symlinked config paths require a cgo-enabled Darwin build"))
	}
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), displayPath)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func readPinnedSymlink(*os.File) (string, error) {
	return "", errors.Join(ErrUnsafeConfigTarget,
		errors.New("symlinked config paths require a cgo-enabled Darwin build"))
}

func openPinnedReadableFinal(
	_ *os.File,
	_ string,
	pinned *os.File,
	pinnedInfo fs.FileInfo,
	displayPath string,
) (*os.File, fs.FileInfo, error) {
	fd, err := unix.Dup(int(pinned.Fd()))
	if err != nil {
		return nil, nil, errors.Join(ErrUnsafeConfigTarget, err)
	}
	return os.NewFile(uintptr(fd), displayPath), pinnedInfo, nil
}
