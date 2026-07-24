//go:build windows

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/fileutil"
)

// ensureConfigParentDirectories pins the nearest existing ancestor and then
// creates, validates, and pins each missing directory one component at a time.
// A concurrently inserted reparse point is rejected before the next component
// can be created through it.
func ensureConfigParentDirectories(path string, expectedAncestorIdentity ...string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve Windows config path: %w", err)
	}
	dir := filepath.Dir(filepath.Clean(absolute))
	ancestor := dir
	var missing []string
	for {
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if !info.IsDir() {
				return errors.Join(ErrUnsafeConfigTarget,
					fmt.Errorf("Windows config parent %s is not a directory", ancestor))
			}
			break
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return statErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return statErr
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = parent
	}

	authority, err := pinWindowsConfigParent(filepath.Join(ancestor, ".config-parent-anchor"))
	if err != nil {
		return err
	}
	defer func() { _ = authority.Release() }()
	if len(expectedAncestorIdentity) > 0 && expectedAncestorIdentity[0] != "" {
		opened, openErr := os.Open(ancestor)
		if openErr != nil {
			return openErr
		}
		info, statErr := opened.Stat()
		if statErr != nil {
			_ = opened.Close()
			return statErr
		}
		identity, ok := openedFileIdentity(opened, info)
		_ = opened.Close()
		if !ok || identity != expectedAncestorIdentity[0] {
			return errors.Join(ErrConfigConflict,
				errors.New("config ancestor changed before creating parent directories"))
		}
	}
	current := ancestor
	for index := len(missing) - 1; index >= 0; index-- {
		current = filepath.Join(current, missing[index])
		if err := fileutil.SecureMkdirAll(current, 0o700); err != nil {
			return fmt.Errorf("create Windows config directory %s: %w", current, err)
		}
		handle, err := openWindowsAuthorityDirectory(current)
		if err != nil {
			return err
		}
		authority.handles = append(authority.handles, handle)
	}
	return nil
}
