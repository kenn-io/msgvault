//go:build windows

package requestsign

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func privateFilePermissions(file *os.File, _ os.FileInfo) error {
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current user token: %w", err)
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read private file security descriptor: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read private file owner: %w", err)
	}
	if owner == nil || !owner.Equals(token.User.Sid) {
		return errors.New("private file must be owned by the current user")
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read private file DACL: %w", err)
	}
	if acl == nil {
		return errors.New("private file must have an owner-only DACL")
	}
	for index := range int(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, uint32(index), &ace); err != nil {
			return fmt.Errorf("read private file ACL entry %d: %w", index, err)
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("private file has unsupported ACL entries")
		}
		// #nosec G103 -- GetAce returns an OS-validated ACE with the SID immediately after SidStart.
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Mask != 0 && !sid.Equals(token.User.Sid) {
			return errors.New("private file grants access to another identity")
		}
	}
	return nil
}
