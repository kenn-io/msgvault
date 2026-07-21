//go:build linux

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func openPinnedPathEntry(parent *os.File, name, displayPath string) (*os.File, fs.FileInfo, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
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

func readPinnedSymlink(file *os.File) (string, error) {
	buffer := make([]byte, 256)
	for {
		n, err := unix.Readlinkat(int(file.Fd()), "", buffer)
		if err != nil {
			return "", err
		}
		if n < len(buffer) {
			return string(buffer[:n]), nil
		}
		buffer = make([]byte, len(buffer)*2)
	}
}

func openPinnedReadableFinal(
	parent *os.File,
	name string,
	pinned *os.File,
	pinnedInfo fs.FileInfo,
	displayPath string,
) (*os.File, fs.FileInfo, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open readable pinned config: %w", err)
	}
	readable := os.NewFile(uintptr(fd), displayPath)
	info, err := readable.Stat()
	if err != nil {
		_ = readable.Close()
		return nil, nil, fmt.Errorf("stat readable pinned config: %w", err)
	}
	pinnedIdentity, pinnedOK := openedFileIdentity(pinned, pinnedInfo)
	readableIdentity, readableOK := openedFileIdentity(readable, info)
	if !pinnedOK || !readableOK || pinnedIdentity != readableIdentity {
		_ = readable.Close()
		return nil, nil, errors.Join(ErrConfigConflict,
			errors.New("final config entry changed before readable open"))
	}
	return readable, info, nil
}
