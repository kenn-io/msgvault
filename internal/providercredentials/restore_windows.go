//go:build windows

package providercredentials

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func restorePreflight(dir string, selected map[string]struct{}) error {
	if err := (nativePermissions{}).prepareDirectory(dir); err != nil {
		return fmt.Errorf("inspect restored token directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("inspect restored token directory: %w", err)
	}
	for _, entry := range entries {
		if _, ok := selected[entry.Name()]; ok {
			continue
		}
		if entry.IsDir() {
			return fmt.Errorf("%w: ambiguous nested tokens/%s", ErrRestorePermissionsConflict, entry.Name())
		}
		h, err := openSecurityHandle(filepath.Join(dir, entry.Name()), entry.IsDir(), windows.READ_CONTROL)
		if err != nil {
			return fmt.Errorf("%w: tokens/%s: %w", ErrRestorePermissionsConflict, entry.Name(), err)
		}
		affected, checkErr := inheritedAccess(h)
		_ = windows.CloseHandle(h)
		if checkErr != nil {
			return fmt.Errorf("%w: tokens/%s: %w", ErrRestorePermissionsConflict, entry.Name(), checkErr)
		}
		if affected {
			return fmt.Errorf("%w: tokens/%s", ErrRestorePermissionsConflict, entry.Name())
		}
	}
	return nil
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
