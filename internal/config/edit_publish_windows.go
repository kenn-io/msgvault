//go:build windows

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// publishNewConfig uses fail-if-exists MoveFileExW with WRITE_THROUGH. Unlike
// a hard-link-and-delete sequence, this is the strongest namespace durability
// primitive documented by Win32 for publishing a new file.
func publishNewConfig(candidatePath string, retained *os.File, before ConfigFile) (configPublication, error) {
	targetPath := before.Path
	authority, err := pinWindowsConfigParent(targetPath)
	if err != nil {
		return configPublication{}, err
	}
	parent, err := os.Open(filepath.Dir(targetPath))
	if err != nil {
		_ = authority.Release()
		return configPublication{}, err
	}
	info, statErr := parent.Stat()
	if statErr != nil {
		_ = parent.Close()
		_ = authority.Release()
		return configPublication{}, statErr
	}
	identity, ok := openedFileIdentity(parent, info)
	if !ok || identity != before.parentIdentity {
		_ = parent.Close()
		_ = authority.Release()
		return configPublication{}, errors.Join(ErrConfigConflict, errors.New("config parent changed before creation"))
	}
	candidate, candidateErr := readPhysicalConfigSnapshot(candidatePath)
	if candidateErr != nil {
		_ = parent.Close()
		_ = authority.Release()
		return configPublication{}, candidateErr
	}
	retainedInfo, retainErr := retained.Stat()
	if retainErr != nil {
		_ = parent.Close()
		_ = authority.Release()
		return configPublication{}, retainErr
	}
	retainedIdentity, retainedOK := openedFileIdentity(retained, retainedInfo)
	if !retainedOK || retainedIdentity != candidate.identity {
		_ = parent.Close()
		_ = authority.Release()
		return configPublication{}, errors.Join(ErrConfigConflict,
			errors.New("published Windows candidate identity changed before move"))
	}
	from, err := windows.UTF16PtrFromString(candidatePath)
	if err != nil {
		_ = parent.Close()
		_ = authority.Release()
		return configPublication{}, err
	}
	to, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		_ = parent.Close()
		_ = authority.Release()
		return configPublication{}, err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			_ = parent.Close()
			_ = authority.Release()
			return configPublication{}, fs.ErrExist
		}
		_ = parent.Close()
		_ = authority.Release()
		return configPublication{}, fmt.Errorf("publish new config with MoveFileExW: %w", err)
	}
	publication := configPublication{
		candidateRemains: false,
		published:        candidate,
		release: func() error {
			return errors.Join(parent.Close(), authority.Release())
		},
	}
	publication.rollback = func(expected ConfigFile) error {
		current, readErr := readPhysicalConfigSnapshot(targetPath)
		if readErr != nil || !sameConfigVersion(current, expected) {
			return errors.Join(ErrConfigChanged, ErrConfigConflict,
				errors.New("refusing creation rollback because the published config changed"), readErr)
		}
		return retireWindowsConfigArtifact(targetPath, retained)
	}
	return publication, nil
}
