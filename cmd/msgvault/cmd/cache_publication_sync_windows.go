//go:build windows

package cmd

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func syncFile(path string) error {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode cache file path: %w", err)
	}
	// #nosec G703 -- callers pass fixed files inside private transaction/cache roots.
	handle, err := windows.CreateFile(
		path16,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return fmt.Errorf("open cache file for sync: %w", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := windows.FlushFileBuffers(handle); err != nil {
		return fmt.Errorf("flush cache file: %w", err)
	}
	return nil
}

// Windows has no supported equivalent of fsync(2) for directory handles:
// FlushFileBuffers rejects directory handles even when opened with
// FILE_FLAG_BACKUP_SEMANTICS. Regular file bytes are flushed before namespace
// publication. A successful rename is the supported Windows namespace
// boundary available to this transaction. This no-op matches the config
// publication contract without claiming a directory flush occurred.
func syncDirectory(string) error {
	return nil
}
