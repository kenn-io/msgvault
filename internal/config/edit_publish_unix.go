//go:build darwin || linux

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func publishNewConfig(candidatePath string, _ *os.File, before ConfigFile) (configPublication, error) {
	dirfd, err := unix.Open(filepath.Dir(before.Path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return configPublication{}, fmt.Errorf("pin publication directory: %w", err)
	}
	dir := os.NewFile(uintptr(dirfd), filepath.Dir(before.Path))
	dirInfo, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		return configPublication{}, fmt.Errorf("identify publication directory: %w", err)
	}
	parentIdentity, ok := openedFileIdentity(dir, dirInfo)
	if !ok || parentIdentity != before.parentIdentity {
		_ = dir.Close()
		return configPublication{}, errors.Join(ErrConfigConflict, errors.New("config parent changed before creation"))
	}
	candidateName := filepath.Base(candidatePath)
	targetName := filepath.Base(before.Path)
	candidate, err := readPhysicalConfigSnapshotAt(dirfd, candidateName, candidatePath)
	if err != nil {
		_ = dir.Close()
		return configPublication{}, fmt.Errorf("publish config relative to pinned directory: %w", err)
	}
	retained, err := retainConfigEntryAt(dirfd, candidateName, candidatePath, candidate.identity)
	if err != nil {
		_ = dir.Close()
		return configPublication{}, fmt.Errorf("retain published config identity: %w", err)
	}
	if err := renameConfigNoReplaceAt(dirfd, candidateName, targetName); err != nil {
		_ = retained.Close()
		_ = dir.Close()
		return configPublication{}, fmt.Errorf("publish config in pinned directory: %w", err)
	}
	publication := configPublication{
		candidateRemains: false,
		published:        candidate,
		syncDirectory: func() error {
			if err := unix.Fsync(dirfd); err != nil {
				return fmt.Errorf("sync pinned config directory: %w", err)
			}
			return nil
		},
		release: func() error {
			if err := errors.Join(retained.Close(), dir.Close()); err != nil {
				return fmt.Errorf("close pinned config directory: %w", err)
			}
			return nil
		},
	}
	publication.rollback = func(expected ConfigFile) error {
		current, readErr := readPhysicalConfigSnapshotAt(dirfd, targetName, before.Path)
		if readErr != nil || !sameConfigVersion(current, expected) {
			return errors.Join(ErrConfigChanged, ErrConfigConflict,
				errors.New("refusing creation rollback because the published config changed"), readErr)
		}
		return retireConfigArtifactAt(dirfd, targetName, before.Path, retained)
	}
	return publication, nil
}
