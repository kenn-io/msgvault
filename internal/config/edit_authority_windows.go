//go:build windows

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsPathAuthority struct {
	mu      sync.Mutex
	handles []windows.Handle
	closed  bool
}

type windowsFileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

// pinWindowsConfigParent retains every ordinary directory component for the
// duration of a transaction. Each retained handle denies FILE_SHARE_DELETE,
// so no pinned component can be renamed, deleted, or replaced while child
// operations continue through the verified pathname. Write sharing stays
// permitted: publishing renames open the target directory with
// FILE_WRITE_DATA | SYNCHRONIZE (see the FILE_RENAME_INFORMATION contract),
// so a share-read-only pin would make the transaction's own MoveFileExW and
// ReplaceFileW calls fail with a sharing violation. A transient share-read
// probe still rejects directories that already have a write- or
// delete-capable handle open when the pin is taken.
func pinWindowsConfigParent(path string) (*windowsPathAuthority, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve Windows config path: %w", err)
	}
	parent := filepath.Dir(filepath.Clean(absolute))
	volume := filepath.VolumeName(parent)
	root := volume + string(filepath.Separator)
	relative := strings.TrimPrefix(parent, root)
	authority := &windowsPathAuthority{}
	prefix := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			_ = authority.Release()
			return nil, errors.Join(ErrUnsafeConfigTarget, errors.New("Windows config path escapes its volume root"))
		}
		prefix = filepath.Join(prefix, part)
		handle, openErr := openWindowsAuthorityDirectory(prefix)
		if openErr != nil {
			_ = authority.Release()
			return nil, openErr
		}
		authority.handles = append(authority.handles, handle)
	}
	return authority, nil
}

// pinWindowsNearestExistingConfigAncestor retains the ordinary directory
// chain through the nearest existing ancestor of a missing config parent. It
// never creates the missing suffix.
func pinWindowsNearestExistingConfigAncestor(path string) (*windowsPathAuthority, string, string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve Windows config path: %w", err)
	}
	ancestor := filepath.Dir(filepath.Clean(absolute))
	for {
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if !info.IsDir() {
				return nil, "", "", errors.Join(ErrUnsafeConfigTarget,
					fmt.Errorf("Windows config ancestor %s is not a directory", ancestor))
			}
			break
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return nil, "", "", statErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return nil, "", "", statErr
		}
		ancestor = parent
	}
	authority, err := pinWindowsConfigParent(filepath.Join(ancestor, ".config-ancestor-anchor"))
	if err != nil {
		return nil, "", "", err
	}
	opened, err := os.Open(ancestor)
	if err != nil {
		_ = authority.Release()
		return nil, "", "", err
	}
	info, err := opened.Stat()
	if err != nil {
		_ = opened.Close()
		_ = authority.Release()
		return nil, "", "", err
	}
	identity, ok := openedFileIdentity(opened, info)
	_ = opened.Close()
	if !ok {
		_ = authority.Release()
		return nil, "", "", errors.Join(ErrUnsafeConfigTarget,
			errors.New("config ancestor identity is unavailable"))
	}
	return authority, filepath.Clean(absolute), identity, nil
}

func openWindowsAuthorityDirectory(path string) (windows.Handle, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode Windows config directory: %w", err)
	}
	// The share-read probe fails while any existing handle holds write or
	// delete access, so a transaction never starts under a concurrent
	// directory writer.
	probe, err := openWindowsDirectoryHandle(encoded, windows.FILE_SHARE_READ)
	if err != nil {
		return 0, fmt.Errorf("pin Windows config directory %s: %w", path, err)
	}
	// The durable pin adds FILE_SHARE_WRITE because publishing renames open
	// the target directory with FILE_WRITE_DATA. Excluding FILE_SHARE_DELETE
	// still blocks rename, deletion, and replacement of the component. The
	// probe closes only after the durable pin exists, so the directory is
	// never unpinned in between.
	handle, err := openWindowsDirectoryHandle(encoded, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	_ = windows.CloseHandle(probe)
	if err != nil {
		return 0, fmt.Errorf("pin Windows config directory %s: %w", path, err)
	}
	var attributes windowsFileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&attributes)),
		uint32(unsafe.Sizeof(attributes)),
	); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("inspect pinned Windows config directory %s: %w", path, err)
	}
	if attributes.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		attributes.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return 0, errors.Join(ErrUnsafeConfigTarget,
			fmt.Errorf("Windows config directory %s is not an ordinary directory", path))
	}
	return handle, nil
}

func openWindowsDirectoryHandle(encoded *uint16, share uint32) (windows.Handle, error) {
	return windows.CreateFile(
		encoded,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		share,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
}

func (authority *windowsPathAuthority) Release() error {
	if authority == nil {
		return nil
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return nil
	}
	authority.closed = true
	var errs []error
	for index := len(authority.handles) - 1; index >= 0; index-- {
		if err := windows.CloseHandle(authority.handles[index]); err != nil {
			errs = append(errs, fmt.Errorf("release Windows config directory authority: %w", err))
		}
	}
	return errors.Join(errs...)
}

func rejectWindowsReparseComponents(path string) error {
	authority, err := pinWindowsConfigParent(path)
	if err != nil {
		return err
	}
	return authority.Release()
}
