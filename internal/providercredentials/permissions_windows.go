//go:build windows

package providercredentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

type nativePermissions struct{}

const providerCredentialFileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1FF

func (nativePermissions) secureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	handle, err := openSecurityHandle(path, true, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck // security result is authoritative
	return secureOwnerOnlyHandle(handle)
}

func (nativePermissions) verifyDirectory(path string) error {
	handle, err := openSecurityHandle(path, true, windows.READ_CONTROL)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck // read-only handle
	return verifyOwnerOnlyHandle(handle)
}

func (nativePermissions) secureFile(file *os.File) error {
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	// os.OpenFile and os.CreateTemp do not request WRITE_DAC on Windows, so
	// their handles cannot publish the owner-only ACL. Reopen the file through
	// the already secured token directory with the exact security rights needed.
	handle, err := openSecurityHandle(file.Name(), false, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck // security result is authoritative
	return secureOwnerOnlyHandle(handle)
}

func (nativePermissions) verifyFile(file *os.File) error {
	return verifyOwnerOnlyHandle(windows.Handle(file.Fd()))
}

func openSecurityHandle(path string, directory bool, access uint32) (windows.Handle, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(path16, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck // original error returned
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(handle) //nolint:errcheck // rejection is authoritative
		return 0, errors.New("provider credential path must not be a reparse point")
	}
	return handle, nil
}

func secureOwnerOnlyHandle(handle windows.Handle) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current user SID: %w", err)
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build owner-only provider credential DACL: %w", err)
	}
	securityInfo := windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.SECURITY_INFORMATION(securityInfo), nil, nil, acl, nil); err != nil {
		return fmt.Errorf("set owner-only provider credential DACL: %w", err)
	}
	return verifyOwnerOnlyHandleForUser(handle, user.User.Sid)
}

func verifyOwnerOnlyHandle(handle windows.Handle) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current user SID: %w", err)
	}
	return verifyOwnerOnlyHandleForUser(handle, user.User.Sid)
}

func verifyOwnerOnlyHandleForUser(handle windows.Handle, user *windows.SID) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read provider credential DACL: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return errors.New("provider credential owner is unavailable")
	}
	if err := verifyWindowsOwner(owner, user); err != nil {
		return err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("provider credential DACL permits inherited access")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return errors.New("provider credential DACL is unavailable")
	}
	type aclHeader struct {
		Revision byte
		Sbz1     byte
		Size     uint16
		AceCount uint16
		Sbz2     uint16
	}
	if (*aclHeader)(unsafe.Pointer(dacl)).AceCount != 1 {
		return errors.New("provider credential DACL must contain exactly one access entry")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return err
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
		(ace.Mask != windows.GENERIC_ALL && ace.Mask != providerCredentialFileAllAccess) ||
		ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
		return errors.New("provider credential DACL does not grant exactly owner full control")
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSID.Equals(user) {
		return errors.New("provider credential DACL grants another principal")
	}
	return nil
}

func verifyWindowsOwner(owner, user *windows.SID) error {
	if owner.Equals(user) {
		return nil
	}
	if owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		member, err := windows.Token(0).IsMember(owner)
		if err != nil {
			return err
		}
		if member {
			return nil
		}
	}
	return errors.New("provider credential owner is not the current user")
}

func openStoreFile(path string) (*os.File, error) {
	handle, err := openSecurityHandle(path, false, windows.GENERIC_READ|windows.READ_CONTROL)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func withStoreLock(tokenDir string, fn func() error) error {
	path := filepath.Join(tokenDir, ".provider-credentials.lock")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open provider credential lock: %w", err)
	}
	defer file.Close() //nolint:errcheck // lock result is authoritative
	if err := (nativePermissions{}).secureFile(file); err != nil {
		return fmt.Errorf("secure provider credential lock: %w", err)
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK,
		0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("lock provider credential store: %w", err)
	}
	defer windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped) //nolint:errcheck
	return fn()
}

func replaceStoreFile(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// MoveFileEx with WRITE_THROUGH is the Windows namespace durability boundary;
// Windows does not support flushing directory handles.
func syncStoreDirectory(string) error { return nil }
