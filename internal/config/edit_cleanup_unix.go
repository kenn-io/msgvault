//go:build darwin || linux

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// retireConfigArtifactAt moves the named entry to an unpredictable recovery
// name and proves it is the retained transaction object. The recovery file and
// its contents are deliberately preserved: pathname deletion and descriptor
// truncation cannot prove that no user-created hardlink references the inode.
func retireConfigArtifactAt(dirfd int, name, displayPath string, expected *os.File) error {
	return retireConfigArtifactAtWithHook(dirfd, name, displayPath, expected, nil)
}

func retireConfigArtifactAtWithHook(
	dirfd int,
	name, displayPath string,
	expected *os.File,
	beforeFinalVerification func(string) error,
) error {
	expectedInfo, err := expected.Stat()
	if err != nil {
		return fmt.Errorf("identify retained config retirement entry: %w", err)
	}
	expectedIdentity, expectedOK := openedFileIdentity(expected, expectedInfo)
	if !expectedOK {
		return errors.Join(ErrConfigChanged, errors.New("retained config retirement identity is unavailable"))
	}
	for range 128 {
		quarantineName, nameErr := newConfigRetiredName()
		if nameErr != nil {
			return nameErr
		}
		if err := renameConfigNoReplaceAt(dirfd, name, quarantineName); err != nil {
			if errors.Is(err, unix.ENOENT) {
				return errors.Join(ErrConfigChanged, ErrConfigConflict,
					errors.New("config retirement entry disappeared before quarantine"))
			}
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return fmt.Errorf("quarantine config retirement entry: %w", err)
		}
		quarantinePath := filepath.Join(filepath.Dir(displayPath), quarantineName)
		quarantined, quarantinedInfo, inspectErr := openConfigEntryAt(dirfd, quarantineName, quarantinePath)
		var quarantinedIdentity string
		var quarantinedOK bool
		if inspectErr == nil {
			quarantinedIdentity, quarantinedOK = openedFileIdentity(quarantined, quarantinedInfo)
		}
		if inspectErr == nil && quarantinedOK && quarantinedIdentity == expectedIdentity {
			_ = quarantined.Close()
			if beforeFinalVerification != nil {
				if err := beforeFinalVerification(quarantinePath); err != nil {
					return errors.Join(ErrConfigChanged, fmt.Errorf("before config retirement verification: %w", err))
				}
			}
			current, currentInfo, currentErr := openConfigEntryAt(dirfd, quarantineName, quarantinePath)
			var currentIdentity string
			var currentOK bool
			if currentErr == nil {
				currentIdentity, currentOK = openedFileIdentity(current, currentInfo)
				_ = current.Close()
			}
			if currentErr == nil && currentOK && currentIdentity == expectedIdentity {
				return nil
			}
			inspectErr = currentErr
		} else if quarantined != nil {
			_ = quarantined.Close()
		}

		restoreErr := renameConfigNoReplaceAt(dirfd, quarantineName, name)
		if restoreErr == nil {
			return errors.Join(ErrConfigChanged, ErrConfigConflict,
				errors.New("refusing to retire a substituted config entry"), inspectErr)
		}
		return errors.Join(ErrConfigChanged, ErrConfigConflict,
			errors.New("refusing to retire a substituted config entry"), inspectErr,
			fmt.Errorf("restore substituted entry: %w", restoreErr),
			fmt.Errorf("config recovery artifact preserved at %s", quarantinePath))
	}
	return errors.Join(ErrConfigChanged, errors.New("could not allocate config recovery artifact"))
}

func openConfigEntryAt(dirfd int, name, displayPath string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open config cleanup entry: %w", err)
	}
	file := os.NewFile(uintptr(fd), displayPath)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func retainConfigEntryAt(dirfd int, name, displayPath, expectedIdentity string) (*os.File, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open retained config retirement entry: %w", err)
	}
	file := os.NewFile(uintptr(fd), displayPath)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("identify retained config retirement entry: %w", err)
	}
	identity, ok := openedFileIdentity(file, info)
	if !ok || identity != expectedIdentity {
		_ = file.Close()
		return nil, errors.Join(ErrConfigConflict, errors.New("config cleanup identity changed before retention"))
	}
	return file, nil
}
