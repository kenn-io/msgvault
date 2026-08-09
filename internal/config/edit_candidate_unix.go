//go:build darwin || linux

package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func createConfigCandidate(dir string) (configCandidate, error) {
	dirfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return configCandidate{}, fmt.Errorf("pin candidate directory: %w", err)
	}
	for range 128 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			_ = unix.Close(dirfd)
			return configCandidate{}, fmt.Errorf("generate candidate name: %w", err)
		}
		name := ".config-edit-" + hex.EncodeToString(random[:]) + ".toml.tmp"
		fd, openErr := unix.Openat(dirfd, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
		if openErr == unix.EEXIST {
			continue
		}
		if openErr != nil {
			_ = unix.Close(dirfd)
			return configCandidate{}, fmt.Errorf("create candidate relative to pinned directory: %w", openErr)
		}
		file := os.NewFile(uintptr(fd), filepath.Join(dir, name))
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			_ = unix.Close(dirfd)
			return configCandidate{}, fmt.Errorf("identify config candidate: %w", statErr)
		}
		_, ok := openedFileIdentity(file, info)
		if !ok {
			_ = file.Close()
			_ = unix.Close(dirfd)
			return configCandidate{}, errors.New("config candidate identity is unavailable")
		}
		retainedFD, duplicateErr := unix.Dup(fd)
		if duplicateErr != nil {
			_ = file.Close()
			_ = unix.Close(dirfd)
			return configCandidate{}, fmt.Errorf("retain config candidate identity: %w", duplicateErr)
		}
		unix.CloseOnExec(retainedFD)
		retained := os.NewFile(uintptr(retainedFD), file.Name())
		return configCandidate{
			file:     file,
			retained: retained,
			path:     file.Name(),
			cleanup: func() error {
				return retireConfigArtifactAt(dirfd, name, file.Name(), retained)
			},
			release: func() error { return errors.Join(retained.Close(), unix.Close(dirfd)) },
		}, nil
	}
	_ = unix.Close(dirfd)
	return configCandidate{}, errors.New("could not allocate a unique config candidate")
}
