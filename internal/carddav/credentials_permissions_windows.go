//go:build windows

package carddav

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type nativeCredentialPermissions struct{}

const cardDAVFileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1FF

func (nativeCredentialPermissions) secureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	handle, err := openCardDAVSecurityHandle(path, true, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck // security result is returned below
	return secureCardDAVHandle(handle)
}

func (nativeCredentialPermissions) secureFile(file *os.File) error {
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	handle, err := openCardDAVSecurityHandle(file.Name(), false, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck // security result is returned below
	return secureCardDAVHandle(handle)
}

func (nativeCredentialPermissions) verifyFile(file *os.File) error {
	handle, err := openCardDAVSecurityHandle(file.Name(), false, windows.READ_CONTROL)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck // security result is returned below
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current user SID: %w", err)
	}
	return verifyCardDAVHandleOwnerOnly(handle, user.User.Sid)
}

func openCardDAVSecurityHandle(path string, directory bool, access uint32) (windows.Handle, error) {
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
		return 0, fmt.Errorf("open CardDAV token security handle: %w", err)
	}
	return handle, nil
}

func secureCardDAVHandle(handle windows.Handle) error {
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
		return fmt.Errorf("build owner-only CardDAV DACL: %w", err)
	}
	securityInfo := windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.SECURITY_INFORMATION(securityInfo), nil, nil, acl, nil); err != nil {
		return fmt.Errorf("set owner-only CardDAV DACL: %w", err)
	}
	return verifyCardDAVHandleOwnerOnly(handle, user.User.Sid)
}

func verifyCardDAVHandleOwnerOnly(handle windows.Handle, user *windows.SID) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read CardDAV DACL: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("read CardDAV owner: %w", err)
	}
	if err := verifyCardDAVOwner(owner, user); err != nil {
		return err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read CardDAV DACL control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("CardDAV DACL permits inherited access")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("read CardDAV DACL entries: %w", err)
	}
	type aclHeader struct {
		Revision byte
		Sbz1     byte
		Size     uint16
		AceCount uint16
		Sbz2     uint16
	}
	if (*aclHeader)(unsafe.Pointer(dacl)).AceCount != 1 {
		return errors.New("CardDAV DACL must contain exactly one access entry")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return fmt.Errorf("read CardDAV owner ACE: %w", err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
		(ace.Mask != windows.GENERIC_ALL && ace.Mask != cardDAVFileAllAccess) {
		return errors.New("CardDAV DACL does not grant exactly full control")
	}
	if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
		return errors.New("CardDAV DACL contains inherited access")
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSID.Equals(user) {
		return errors.New("CardDAV DACL grants a principal other than the current user")
	}
	return nil
}

func verifyCardDAVOwner(owner, user *windows.SID) error {
	if owner.Equals(user) {
		return nil
	}
	if owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		member, err := windows.Token(0).IsMember(owner)
		if err != nil {
			return fmt.Errorf("check Administrators membership: %w", err)
		}
		if member {
			return nil
		}
	}
	return errors.New("CardDAV token owner is not the current user")
}
