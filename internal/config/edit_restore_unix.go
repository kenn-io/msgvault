//go:build darwin || linux

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func retireExactConfigForMissingRestore(current, before ConfigFile) error {
	if current.retained == nil {
		return errors.Join(ErrConfigChanged, errors.New("config rollback identity is not retained"))
	}
	dirPath := filepath.Dir(before.Path)
	dirfd, err := unix.Open(dirPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("pin config rollback directory: %w", err)
	}
	dir := os.NewFile(uintptr(dirfd), dirPath)
	defer func() { _ = dir.Close() }()
	info, err := dir.Stat()
	if err != nil {
		return fmt.Errorf("identify config rollback directory: %w", err)
	}
	identity, ok := openedFileIdentity(dir, info)
	if !ok || identity != before.parentIdentity {
		return errors.Join(ErrConfigConflict, errors.New("config rollback parent changed"))
	}
	if err := retireConfigArtifactAt(dirfd, filepath.Base(before.Path), before.Path, current.retained); err != nil {
		return err
	}
	if err := unix.Fsync(dirfd); err != nil {
		return errors.Join(ErrConfigChanged, fmt.Errorf("sync missing config rollback: %w", err))
	}
	return nil
}
