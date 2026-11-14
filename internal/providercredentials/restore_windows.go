//go:build windows

package providercredentials

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrRestorePermissionsConflict means an omitted entry cannot safely retain its access.
var ErrRestorePermissionsConflict = errors.New("restored token permissions conflict with an omitted entry")

// SecureRestored protects snapshot-selected token files while preserving omitted entries.
func SecureRestored(dir string, restored []string) error {
	var ancestorNames []string
	for _, name := range restored {
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			ancestorNames = append(ancestorNames, parent)
		}
	}
	selected, err := newOrdinalPaths(restored)
	if err != nil {
		return err
	}
	ancestors, err := newOrdinalPaths(ancestorNames)
	if err != nil {
		return err
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

func restorePreflight(dir string, selected, ancestors ordinalPaths, rootAffected bool) ([]string, error) {
	var directories []string
	affected := map[string]bool{".": rootAffected}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
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
		parentAffected := affected[filepath.Dir(rel)]
		selectedFile, err := selected.contains(rel)
		if err != nil {
			return fmt.Errorf("compare tokens/%s: %w", rel, err)
		}
		selectedAncestor, err := ancestors.contains(rel)
		if err != nil {
			return fmt.Errorf("compare tokens/%s: %w", rel, err)
		}
		if !parentAffected && !selectedFile && !selectedAncestor {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return fmt.Errorf("inspect tokens/%s: reparse point or non-regular entry", rel)
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
		protected := control&windows.SE_DACL_PROTECTED != 0
		if entry.IsDir() {
			if selectedAncestor && verifyOwnerOnlyHandle(h, false) != nil {
				if err := verifyHandleOwner(h); err != nil {
					return fmt.Errorf("inspect tokens/%s: %w", rel, err)
				}
				directories = append(directories, rel)
				affected[rel] = true
				return nil
			}
			affected[rel] = parentAffected && !protected
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
			return fmt.Errorf("inspect tokens/%s: %w", rel, err)
		}
		if inherited {
			return fmt.Errorf("%w: tokens/%s", ErrRestorePermissionsConflict, rel)
		}
		return nil
	})
	return directories, err
}

var compareStringOrdinal = windows.NewLazySystemDLL("kernel32.dll").NewProc("CompareStringOrdinal")

type ordinalPaths [][]uint16

func compareOrdinal(left, right []uint16) (int, error) {
	// #nosec G103 -- CompareStringOrdinal reads both UTF-16 buffers during the call.
	result, _, err := compareStringOrdinal.Call(
		uintptr(unsafe.Pointer(&left[0])), uintptr(len(left)-1),
		uintptr(unsafe.Pointer(&right[0])), uintptr(len(right)-1), 1,
	)
	if result == 0 {
		return 0, fmt.Errorf("compare Windows paths: %w", err)
	}
	return int(result) - 2, nil
}

func newOrdinalPaths(names []string) (ordinalPaths, error) {
	paths := make(ordinalPaths, 0, len(names))
	for _, name := range names {
		path, err := windows.UTF16FromString(name)
		if err != nil {
			return nil, fmt.Errorf("encode Windows path: %w", err)
		}
		paths = append(paths, path)
	}
	var compareErr error
	sort.Slice(paths, func(i, j int) bool {
		order, err := compareOrdinal(paths[i], paths[j])
		if err != nil {
			compareErr = err
		}
		return order < 0
	})
	return paths, compareErr
}

func (paths ordinalPaths) contains(name string) (bool, error) {
	path, err := windows.UTF16FromString(name)
	if err != nil {
		return false, fmt.Errorf("encode Windows path: %w", err)
	}
	var compareErr error
	index := sort.Search(len(paths), func(i int) bool {
		order, err := compareOrdinal(paths[i], path)
		if err != nil {
			compareErr = err
		}
		return order >= 0
	})
	if compareErr != nil || index == len(paths) {
		return false, compareErr
	}
	order, err := compareOrdinal(paths[index], path)
	return order == 0 && err == nil, err
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
