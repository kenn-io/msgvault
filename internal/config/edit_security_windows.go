//go:build windows

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsDirectoryDurabilityBoundary struct{}

// Windows has no supported equivalent of fsync(2) for directory handles:
// FlushFileBuffers rejects directory handles even when opened with
// FILE_FLAG_BACKUP_SEMANTICS. Candidate bytes are flushed before publication,
// ReplaceFileW (existing files) and write-through MoveFileExW (new files) are
// the Windows namespace publication boundaries. This successful no-op keeps
// the transaction supported without claiming a directory flush occurred.
func openConfigDirectoryForSync(string) (syncDirectoryHandle, error) {
	return windowsDirectoryDurabilityBoundary{}, nil
}

func (windowsDirectoryDurabilityBoundary) Sync() error  { return nil }
func (windowsDirectoryDurabilityBoundary) Close() error { return nil }

func secureConfigCandidate(file *os.File, _ string, mode fs.FileMode) error {
	if err := file.Chmod(mode); err != nil {
		return err
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current user SID: %w", err)
	}
	acl, err := ownerOnlyConfigACL(user.User.Sid)
	if err != nil {
		return err
	}
	securityInfo := windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	if err := windows.SetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.SECURITY_INFORMATION(securityInfo),
		nil,
		nil,
		acl,
		nil,
	); err != nil {
		return fmt.Errorf("set owner-only config DACL: %w", err)
	}
	return verifyConfigHandleOwnerOnly(windows.Handle(file.Fd()), user.User.Sid)
}

func ownerOnlyConfigACL(user *windows.SID) (*windows.ACL, error) {
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user),
		},
	}}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return nil, fmt.Errorf("build owner-only config DACL: %w", err)
	}
	return acl, nil
}

func verifyConfigOwnerOnly(path string) error {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		path16,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return verifyConfigHandleOwnerOnly(handle, user.User.Sid)
}

func validateOpenedConfigSecurity(file *os.File) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return ensureConfigHandleOwnerOnly(windows.Handle(file.Fd()), user.User.Sid)
}

// ensureConfigHandleOwnerOnly migrates legacy files created with an inherited
// DACL only after proving the already-opened file is owned by the current user.
// All hardening and verification remains bound to that handle, so a pathname
// substitution cannot redirect the metadata change after ownership proof.
func ensureConfigHandleOwnerOnly(handle windows.Handle, user *windows.SID) error {
	if err := verifyConfigHandleOwnerOnly(handle, user); err == nil {
		return nil
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read legacy config owner: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		return errors.New("legacy config owner is not the current user")
	}
	acl, err := ownerOnlyConfigACL(user)
	if err != nil {
		return err
	}
	securityInfo := windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.SECURITY_INFORMATION(securityInfo), nil, nil, acl, nil); err != nil {
		return fmt.Errorf("harden legacy config DACL: %w", err)
	}
	return verifyConfigHandleOwnerOnly(handle, user)
}

func verifyConfigHandleOwnerOnly(handle windows.Handle, user *windows.SID) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read config DACL: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("read config owner: %w", err)
	}
	if !owner.Equals(user) {
		return errors.New("config owner is not the current user")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read config DACL control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("config DACL permits inherited access")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("read config DACL entries: %w", err)
	}
	type aclHeader struct {
		Revision byte
		Sbz1     byte
		Size     uint16
		AceCount uint16
		Sbz2     uint16
	}
	if (*aclHeader)(unsafe.Pointer(dacl)).AceCount != 1 {
		return errors.New("config DACL must contain exactly one access entry")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return fmt.Errorf("read config owner ACE: %w", err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask != windows.GENERIC_ALL {
		return errors.New("config DACL does not grant exactly GENERIC_ALL")
	}
	if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
		return errors.New("config DACL contains inherited access")
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSID.Equals(user) {
		return errors.New("config DACL grants a principal other than the current user")
	}
	return nil
}
