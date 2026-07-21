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

func retainWindowsConfigArtifact(path, expectedIdentity string) (*os.File, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode Windows config retirement path: %w", err)
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("retain Windows config retirement entry: %w", err)
	}
	file := os.NewFile(uintptr(handle), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("identify Windows config retirement entry: %w", err)
	}
	identity, ok := openedFileIdentity(file, info)
	if !ok || identity != expectedIdentity {
		_ = file.Close()
		return nil, errors.Join(ErrConfigConflict,
			errors.New("Windows config retirement identity changed before retention"))
	}
	return file, nil
}

func retireWindowsConfigArtifact(path string, expected *os.File) error {
	return retireWindowsConfigArtifactWithHook(path, expected, nil)
}

func retireWindowsConfigArtifactWithHook(
	path string,
	expected *os.File,
	beforeFinalVerification func(string) error,
) error {
	authority, err := pinWindowsConfigParent(path)
	if err != nil {
		return err
	}
	defer func() { _ = authority.Release() }()
	expectedInfo, err := expected.Stat()
	if err != nil {
		return fmt.Errorf("identify retained Windows config retirement entry: %w", err)
	}
	expectedIdentity, ok := openedFileIdentity(expected, expectedInfo)
	if !ok {
		return errors.Join(ErrConfigChanged, errors.New("retained Windows config retirement identity is unavailable"))
	}
	for range 128 {
		name, nameErr := newConfigRetiredName()
		if nameErr != nil {
			return nameErr
		}
		quarantinePath := filepath.Join(filepath.Dir(path), name)
		moveErr := moveFileWriteThrough(path, quarantinePath)
		if errors.Is(moveErr, windows.ERROR_FILE_NOT_FOUND) || errors.Is(moveErr, windows.ERROR_PATH_NOT_FOUND) {
			return errors.Join(ErrConfigChanged, ErrConfigConflict,
				errors.New("Windows config retirement entry disappeared before quarantine"))
		}
		if errors.Is(moveErr, windows.ERROR_ALREADY_EXISTS) || errors.Is(moveErr, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if moveErr != nil {
			return fmt.Errorf("quarantine Windows config retirement entry: %w", moveErr)
		}

		quarantined, inspectErr := openWindowsIdentityEntry(quarantinePath)
		var quarantineIdentity string
		var quarantineOK bool
		if inspectErr == nil {
			quarantineInfo, statErr := quarantined.Stat()
			if statErr == nil {
				quarantineIdentity, quarantineOK = openedFileIdentity(quarantined, quarantineInfo)
			} else {
				inspectErr = statErr
			}
			_ = quarantined.Close()
		}
		if inspectErr == nil && quarantineOK && quarantineIdentity == expectedIdentity {
			if beforeFinalVerification != nil {
				if err := beforeFinalVerification(quarantinePath); err != nil {
					return errors.Join(ErrConfigChanged,
						fmt.Errorf("before Windows config retirement verification: %w", err))
				}
			}
			current, currentErr := openWindowsIdentityEntry(quarantinePath)
			var currentIdentity string
			var currentOK bool
			if currentErr == nil {
				currentInfo, statErr := current.Stat()
				if statErr == nil {
					currentIdentity, currentOK = openedFileIdentity(current, currentInfo)
				} else {
					currentErr = statErr
				}
				_ = current.Close()
			}
			if currentErr == nil && currentOK && currentIdentity == expectedIdentity {
				return nil
			}
			inspectErr = currentErr
		}

		restoreErr := moveFileWriteThrough(quarantinePath, path)
		if restoreErr == nil {
			return errors.Join(ErrConfigChanged, ErrConfigConflict,
				errors.New("refusing to retire a substituted Windows config entry"), inspectErr)
		}
		return errors.Join(ErrConfigChanged, ErrConfigConflict,
			errors.New("refusing to retire a substituted Windows config entry"), inspectErr,
			fmt.Errorf("restore substituted Windows config entry: %w", restoreErr),
			fmt.Errorf("config recovery artifact preserved at %s", quarantinePath))
	}
	return errors.Join(ErrConfigChanged, errors.New("could not allocate Windows config recovery artifact"))
}

func openWindowsIdentityEntry(path string) (*os.File, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, fs.ErrNotExist
		}
		return nil, fmt.Errorf("open Windows config retirement entry: %w", err)
	}
	return os.NewFile(uintptr(handle), path), nil
}
