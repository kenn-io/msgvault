//go:build windows

package providercredentials

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// ErrRestorePermissionsConflict means an omitted entry cannot safely retain its access.
var ErrRestorePermissionsConflict = errors.New("restored token permissions conflict with an omitted entry")

// SecureRestored protects snapshot-selected token files while preserving omitted entries.
func SecureRestored(dir string, restored []string) error {
	if len(restored) == 0 {
		return nil
	}
	var ancestors []string
	for _, name := range restored {
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			ancestors = append(ancestors, parent)
		}
	}
	return withDirectoryMutex(dir, func(directory windows.Handle) error {
		if err := verifyHandleOwner(directory); err != nil {
			return fmt.Errorf("inspect restored token directory: %w", err)
		}
		for _, name := range restored {
			if err := secureFileACL(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("secure restored token %q: %w", name, err)
			}
		}
		if verifyOwnerOnlyHandle(directory, false) != nil {
			if err := restorePreflight(dir, restored, ancestors); err != nil {
				return err
			}
			return secureOwnerOnlyHandle(directory)
		}
		return nil
	})
}

func restorePreflight(dir string, selected, ancestors []string) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("inspect tokens: %w", walkErr)
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("inspect tokens: %w", err)
		}
		if containsPath(selected, rel) {
			return nil
		}
		h, err := openSecurityHandle(path, entry.IsDir(), windows.READ_CONTROL)
		if err != nil {
			return fmt.Errorf("inspect tokens/%s: %w", rel, err)
		}
		defer windows.CloseHandle(h) //nolint:errcheck // inspection result is authoritative
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("inspect tokens/%s: %w", rel, err)
		}
		control, _, err := sd.Control()
		if err != nil {
			return fmt.Errorf("inspect tokens/%s: %w", rel, err)
		}
		if entry.IsDir() && control&windows.SE_DACL_PROTECTED != 0 {
			return fs.SkipDir
		}
		if containsPath(ancestors, rel) {
			return nil
		}
		inherited, err := inheritedAccess(sd)
		if err != nil {
			return fmt.Errorf("inspect tokens/%s: %w", rel, err)
		}
		if inherited {
			return fmt.Errorf("%w: tokens/%s", ErrRestorePermissionsConflict, rel)
		}
		return nil
	})
}

// containsPath matches snapshot paths without regard to case.
func containsPath(names []string, name string) bool {
	for _, candidate := range names {
		if strings.EqualFold(candidate, name) {
			return true
		}
	}
	return false
}

func inheritedAccess(sd *windows.SECURITY_DESCRIPTOR) (bool, error) {
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
