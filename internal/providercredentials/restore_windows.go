//go:build windows

package providercredentials

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
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
	selected := make(map[string]struct{}, len(restored))
	ancestors := make(map[string]struct{})
	for _, name := range restored {
		selected[strings.ToLower(name)] = struct{}{}
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			ancestors[strings.ToLower(parent)] = struct{}{}
		}
	}
	return withDirectoryMutex(dir, func(directory windows.Handle) error {
		if err := verifyHandleOwner(directory); err != nil {
			return fmt.Errorf("inspect restored token directory: %w", err)
		}
		rootAffected := verifyOwnerOnlyHandle(directory, false) != nil
		directories, err := restorePreflight(dir, selected, ancestors, rootAffected)
		if err != nil {
			return err
		}
		for _, name := range restored {
			if err := secureFileACL(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("secure restored token %q: %w", name, err)
			}
		}
		for _, name := range slices.Backward(directories) {
			if err := (nativePermissions{}).secureDirectory(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("secure restored token directory %q: %w", name, err)
			}
		}
		if rootAffected {
			return secureOwnerOnlyHandle(directory)
		}
		return nil
	})
}

func restorePreflight(dir string, selected, ancestors map[string]struct{}, rootAffected bool) ([]string, error) {
	var directories []string
	affected := map[string]bool{".": rootAffected}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
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
		key := strings.ToLower(rel)
		parentAffected := affected[strings.ToLower(filepath.Dir(rel))]
		_, selectedFile := selected[key]
		_, selectedAncestor := ancestors[key]
		if !parentAffected && !selectedFile && !selectedAncestor {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return fmt.Errorf("%w: ambiguous tokens/%s", ErrRestorePermissionsConflict, rel)
		}
		h, err := openSecurityHandle(path, entry.IsDir(), windows.READ_CONTROL)
		if err != nil {
			return fmt.Errorf("%w: tokens/%s: %w", ErrRestorePermissionsConflict, rel, err)
		}
		defer windows.CloseHandle(h) //nolint:errcheck // inspection result is authoritative
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("%w: tokens/%s: %w", ErrRestorePermissionsConflict, rel, err)
		}
		protected, err := protectedDACL(sd)
		if err != nil {
			return fmt.Errorf("%w: tokens/%s: %w", ErrRestorePermissionsConflict, rel, err)
		}
		if entry.IsDir() {
			if selectedAncestor && parentAffected && !protected {
				if err := verifyHandleOwner(h); err != nil {
					return fmt.Errorf("%w: tokens/%s: %w", ErrRestorePermissionsConflict, rel, err)
				}
				directories = append(directories, rel)
				affected[key] = true
				return nil
			}
			affected[key] = parentAffected && !protected
			if protected && !selectedAncestor {
				return fs.SkipDir
			}
		} else if selectedFile {
			return nil
		}
		if !parentAffected || protected {
			return nil
		}
		inherited, err := inheritedAccess(sd)
		if err != nil {
			return fmt.Errorf("%w: tokens/%s: %w", ErrRestorePermissionsConflict, rel, err)
		}
		if inherited {
			return fmt.Errorf("%w: tokens/%s", ErrRestorePermissionsConflict, rel)
		}
		return nil
	})
	return directories, err
}

func protectedDACL(sd *windows.SECURITY_DESCRIPTOR) (bool, error) {
	control, _, err := sd.Control()
	if err != nil {
		return false, fmt.Errorf("read token protection: %w", err)
	}
	return control&windows.SE_DACL_PROTECTED != 0, nil
}

func inheritedAccess(sd *windows.SECURITY_DESCRIPTOR) (bool, error) {
	protected, err := protectedDACL(sd)
	if err != nil || protected {
		return false, err
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
