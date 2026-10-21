//go:build windows

package fileutil

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// FileAllAccess is FILE_ALL_ACCESS. Owner-only ACEs use it instead of
// GENERIC_ALL because Windows splits an inheritable generic ACE in two.
const FileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1FF

// isOwnerOnly returns true if the permission mode grants nothing to group or other.
func isOwnerOnly(perm os.FileMode) bool {
	return perm&0077 == 0
}

// restrictToCurrentUser sets a DACL on path that grants full control only to
// the current user and blocks inherited ACEs. For directories, the DACL
// includes CONTAINER_INHERIT_ACE | OBJECT_INHERIT_ACE so that child files
// and subdirectories automatically inherit the restriction. Errors are
// returned to the caller; the file was already created with the requested
// Unix mode, so callers may treat DACL failures as non-fatal warnings.
func restrictToCurrentUser(path string) error {
	// For directories, enable inheritance so children get the same restriction.
	// For files, NO_INHERITANCE is correct (files don't have children).
	var inherit uint32 = windows.NO_INHERITANCE
	info, statErr := os.Stat(path)
	if statErr == nil && info.IsDir() {
		inherit = windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE
	}

	acl, err := currentUserACL(path, inherit)
	if err != nil {
		return err
	}

	secInfo := windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.SECURITY_INFORMATION(secInfo),
		nil, // owner SID (unchanged)
		nil, // group SID (unchanged)
		acl, // DACL
		nil, // SACL (unchanged)
	)
	if err != nil {
		return fmt.Errorf("fileutil: set DACL on %s: %w", path, err)
	}
	return nil
}

func currentUserACL(path string, inherit uint32) (*windows.ACL, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("fileutil: get current user SID for %s: %w", path, err)
	}
	ea := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: FileAllAccess,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       inherit,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
			},
		},
	}

	acl, err := windows.ACLFromEntries(ea, nil)
	if err != nil {
		return nil, fmt.Errorf("fileutil: build ACL for %s: %w", path, err)
	}

	return acl, nil
}

func protectedDirectoryDescriptor(path string) (*windows.SECURITY_DESCRIPTOR, error) {
	acl, err := currentUserACL(path, windows.CONTAINER_INHERIT_ACE|windows.OBJECT_INHERIT_ACE)
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, fmt.Errorf("fileutil: new security descriptor for %s: %w", path, err)
	}
	if err := descriptor.SetDACL(acl, true, false); err != nil {
		return nil, fmt.Errorf("fileutil: set DACL for %s: %w", path, err)
	}
	if err := descriptor.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, fmt.Errorf("fileutil: protect DACL for %s: %w", path, err)
	}
	return descriptor, nil
}

// SecureWriteFile writes data to the named file, creating it if necessary.
// For owner-only modes, a DACL restricting access to the current user is applied.
// DACL failures are logged as warnings but do not fail the write.
func SecureWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, data, perm); err != nil {
		return err
	}
	if isOwnerOnly(perm) {
		if err := restrictToCurrentUser(path); err != nil {
			slog.Warn("fileutil: best-effort DACL failed", "path", path, "err", err)
		}
	}
	return nil
}

// SecureMkdirAll creates a directory path and all parents that do not yet exist.
// For owner-only modes, each directory it creates starts with a protected DACL
// granting access only to the current user. If Windows rejects that DACL, the
// directory is created normally and restricted afterward, with a warning if
// that also fails. Existing directories, including one another process
// creates first, keep their permissions.
func SecureMkdirAll(path string, perm os.FileMode) error {
	if !isOwnerOnly(perm) || path == "" {
		return os.MkdirAll(path, perm)
	}
	abs := path
	if !strings.HasPrefix(path, `\\?\`) {
		// Normalize ordinary paths while preserving explicit extended paths.
		var err error
		abs, err = filepath.Abs(path)
		if err != nil {
			return err
		}
	}
	var missing []string
	for p := abs; ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err == nil {
			if !info.IsDir() {
				return &os.PathError{Op: "mkdir", Path: p, Err: windows.ERROR_DIRECTORY}
			}
			break
		}
		if !os.IsNotExist(err) || filepath.Dir(p) == p {
			return err
		}
		missing = append(missing, p)
	}
	if len(missing) == 0 {
		return nil
	}

	var attributes *windows.SecurityAttributes
	if descriptor, err := protectedDirectoryDescriptor(abs); err != nil {
		slog.Warn("fileutil: protected directory creation unavailable", "path", abs, "err", err)
	} else {
		attributes = &windows.SecurityAttributes{
			Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
			SecurityDescriptor: descriptor,
		}
	}
	for _, dir := range slices.Backward(missing) {
		if attributes != nil {
			err := createProtectedDirectory(dir, attributes)
			if err == nil || errors.Is(err, windows.ERROR_ALREADY_EXISTS) && isDir(dir) {
				continue
			}
			slog.Warn("fileutil: protected directory creation failed", "path", dir, "err", err)
		}
		if err := os.Mkdir(dir, perm); err != nil {
			if os.IsExist(err) && isDir(dir) {
				continue
			}
			return err
		}
		if err := restrictToCurrentUser(dir); err != nil {
			slog.Warn("fileutil: best-effort DACL failed", "path", dir, "err", err)
		}
	}
	return nil
}

var createDirectory = windows.CreateDirectory

func createProtectedDirectory(dir string, attributes *windows.SecurityAttributes) error {
	path16, err := windows.UTF16PtrFromString(win32CreatePath(dir))
	if err != nil {
		return fmt.Errorf("fileutil: encode directory path %s: %w", dir, err)
	}
	return createDirectory(path16, attributes)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func win32CreatePath(dir string) string {
	// The prefix disables Win32 path normalization, which filepath.Abs has
	// already applied. Use it only where Win32 needs it, as os does.
	if len(dir) >= 248 && !strings.HasPrefix(dir, `\\?\`) && !strings.HasPrefix(dir, `\\.\`) {
		if strings.HasPrefix(dir, `\\`) {
			return `\\?\UNC\` + dir[2:]
		}
		return `\\?\` + dir
	}
	return dir
}

// SecureChmod changes the mode of the named file.
// For owner-only modes, a DACL restricting access to the current user is applied.
// DACL failures are logged as warnings but do not fail the chmod.
func SecureChmod(path string, perm os.FileMode) error {
	if err := os.Chmod(path, perm); err != nil {
		return err
	}
	if isOwnerOnly(perm) {
		if err := restrictToCurrentUser(path); err != nil {
			slog.Warn("fileutil: best-effort DACL failed", "path", path, "err", err)
		}
	}
	return nil
}

// SecureOpenFile opens the named file with specified flag and permissions.
// For owner-only modes when O_CREATE is set, a DACL restricting access to
// the current user is applied — regardless of whether the file already existed.
// This is intentional: all callers write sensitive data (email content,
// attachments) that should be owner-only.
//
// Note: on Windows there is a small TOCTOU window between file creation and
// DACL application because SetNamedSecurityInfo operates by path after the
// file is already open. The window is very brief and exploitation would
// require local access, so this is acceptable for the threat model.
// DACL failures are logged as warnings but do not fail the open.
func SecureOpenFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	if isOwnerOnly(perm) && (flag&os.O_CREATE != 0) {
		if err := restrictToCurrentUser(path); err != nil {
			slog.Warn("fileutil: best-effort DACL failed", "path", path, "err", err)
		}
	}
	return f, nil
}
