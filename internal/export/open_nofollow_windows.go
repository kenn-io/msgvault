//go:build windows

package export

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// openNoFollow opens the final component read-only without traversing a
// reparse point. The caller rejects the reparse descriptor before reading.
func openNoFollow(path string) (*os.File, error) {
	return openNoFollowWindows(path, windows.GENERIC_READ)
}

// openNoFollowDurable includes GENERIC_WRITE because FlushFileBuffers, used by
// os.File.Sync, requires a write-capable Windows handle.
func openNoFollowDurable(path string) (*os.File, error) {
	return openNoFollowWindows(path, windows.GENERIC_READ|windows.GENERIC_WRITE)
}

func openNoFollowWindows(path string, access uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode attachment path: %w", err)
	}
	handle, err := windows.CreateFile(
		name,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("create os file for attachment handle")
	}
	return file, nil
}

func validateNoFollowFileInfo(info os.FileInfo) error {
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return fmt.Errorf("cannot inspect Windows file attributes")
	}
	if attributes.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("is a Windows reparse point")
	}
	return nil
}
