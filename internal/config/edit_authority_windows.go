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

// pinWindowsConfigParent retains every ordinary directory component with
// share-read only. That denies later write/delete opens of the directory
// objects, preventing rename, deletion, or reparse conversion while child
// operations continue through the verified pathname.
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
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
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
