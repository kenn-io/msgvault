//go:build darwin && cgo

package config

/*
#include <unistd.h>
static ssize_t msgvault_freadlink(int fd, char *buf, size_t size) {
	return freadlink(fd, buf, size);
}
*/
import "C"

import (
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPinnedPathEntry(parent *os.File, name, displayPath string) (*os.File, fs.FileInfo, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_SYMLINK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open pinned path entry: %w", err)
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
		n, err := C.msgvault_freadlink(C.int(file.Fd()), (*C.char)(unsafe.Pointer(&buffer[0])), C.size_t(len(buffer)))
		if n < 0 {
			return "", err
		}
		if int(n) < len(buffer) {
			return string(buffer[:n]), nil
		}
		buffer = make([]byte, len(buffer)*2)
	}
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
		return nil, nil, fmt.Errorf("duplicate readable pinned config: %w", err)
	}
	readable := os.NewFile(uintptr(fd), displayPath)
	return readable, pinnedInfo, nil
}
