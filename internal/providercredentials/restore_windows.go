//go:build windows

package providercredentials

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// ErrRestorePermissionsConflict means an omitted entry cannot safely retain its access.
var ErrRestorePermissionsConflict = errors.New("restored token permissions conflict with an omitted entry")

// SecureRestored protects snapshot-selected token files while preserving omitted entries.
func SecureRestored(dir string, restored []string) error {
	if len(restored) == 0 {
		return nil
	}
	selected := make(map[string]struct{}, len(restored))
	ancestors := make(map[string]struct{})
	for _, name := range restored {
		selected[name] = struct{}{}
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			ancestors[parent] = struct{}{}
		}
	}
	return withDirectoryMutex(dir, func(directory windows.Handle) error {
		if err := verifyHandleOwner(directory); err != nil {
			return fmt.Errorf("inspect restored token directory: %w", err)
		}
		if err := restorePreflight(dir, selected, ancestors); err != nil {
			return err
		}
		for _, name := range restored {
			if err := secureRestoredFile(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("secure restored token %q: %w", name, err)
			}
		}
		return secureOwnerOnlyHandle(directory)
	})
}

func restorePreflight(dir string, selected, ancestors map[string]struct{}) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: inspect tokens: %w", ErrRestorePermissionsConflict, walkErr)
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("%w: inspect tokens: %w", ErrRestorePermissionsConflict, err)
		}
		if entry.Type()&fs.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return fmt.Errorf("%w: ambiguous tokens/%s", ErrRestorePermissionsConflict, rel)
		}
		h, err := openSecurityHandle(path, entry.IsDir(), windows.READ_CONTROL)
		if err != nil {
			return fmt.Errorf("%w: tokens/%s: %w", ErrRestorePermissionsConflict, rel, err)
		}
		defer windows.CloseHandle(h) //nolint:errcheck // inspection result is authoritative
		if _, ok := selected[rel]; ok && !entry.IsDir() {
			return nil
		}
		if _, ok := ancestors[rel]; ok && entry.IsDir() {
			return nil
		}
		affected, err := inheritedAccess(h)
		if err != nil {
			return fmt.Errorf("%w: tokens/%s: %w", ErrRestorePermissionsConflict, rel, err)
		}
		if affected {
			return fmt.Errorf("%w: tokens/%s", ErrRestorePermissionsConflict, rel)
		}
		return nil
	})
}

func inheritedAccess(h windows.Handle) (bool, error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, fmt.Errorf("read omitted token permissions: %w", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return false, fmt.Errorf("read omitted token protection: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED != 0 {
		return false, nil
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return false, fmt.Errorf("read omitted token access: %w", err)
	}
	if acl == nil {
		return true, nil
	}
	for i := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return false, fmt.Errorf("read omitted token access entry: %w", err)
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			return true, nil
		}
	}
	return false, nil
}

func secureRestoredFile(path string) error {
	h, err := openSecurityHandle(path, false, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // security result is authoritative
	if err := verifyHandleOwner(h); err != nil {
		return err
	}
	if verifyOwnerOnlyHandle(h, filepath.Base(path) != Filename) == nil {
		return nil
	}
	return secureOwnerOnlyHandle(h)
}
