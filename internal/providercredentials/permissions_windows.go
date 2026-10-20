//go:build windows

package providercredentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"go.kenn.io/msgvault/internal/fileutil"
	"golang.org/x/sys/windows"
)

type nativePermissions struct{}

func (nativePermissions) prepareDirectory(path string) error {
	if err := fileutil.SecureMkdirAll(path, 0o700); err != nil {
		return err
	}
	handle, err := openSecurityHandle(path, true, windows.READ_CONTROL)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck // ownership check is authoritative
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read credential directory owner: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return errors.New("credential directory owner is unavailable")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current user SID: %w", err)
	}
	return verifyWindowsOwner(owner, user.User.Sid)
}

func (nativePermissions) secureDirectory(path string) error {
	if err := fileutil.SecureMkdirAll(path, 0o700); err != nil {
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
	return verifyOwnerOnlyHandle(handle, false)
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
	return verifyOwnerOnlyHandle(windows.Handle(file.Fd()), false)
}

func openSecurityHandle(path string, directory bool, access uint32) (windows.Handle, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode provider credential path: %w", err)
	}
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(path16, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return 0, fmt.Errorf("open provider credential security handle: %w", err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("inspect provider credential handle: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
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
	return verifyOwnerOnlyHandleForUser(handle, user.User.Sid, false)
}

func verifyOwnerOnlyHandle(handle windows.Handle, allowReadOnly bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current user SID: %w", err)
	}
	return verifyOwnerOnlyHandleForUser(handle, user.User.Sid, allowReadOnly)
}

func verifyOwnerOnlyHandleForUser(handle windows.Handle, user *windows.SID, allowReadOnly bool) error {
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
		return fmt.Errorf("read provider credential DACL control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("provider credential DACL permits inherited access")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return errors.New("provider credential DACL is unavailable")
	}
	if dacl.AceCount != 1 {
		return errors.New("provider credential DACL must contain exactly one access entry")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return fmt.Errorf("read provider credential ACE: %w", err)
	}
	fullControl := ace.Mask == windows.GENERIC_ALL || ace.Mask == fileutil.FileAllAccess
	readOnly := allowReadOnly && (ace.Mask == windows.GENERIC_READ || ace.Mask == windows.FILE_GENERIC_READ)
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || (!fullControl && !readOnly) ||
		ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
		return errors.New("provider credential DACL does not grant the required owner access")
	}
	// #nosec G103 -- GetAce supplies an access-allowed ACE whose SidStart is the first word of its contiguous SID.
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
			return fmt.Errorf("check provider credential owner membership: %w", err)
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

func openSecretFile(path string) (*os.File, error) { return openStoreFile(path) }

func verifySecretFile(file *os.File) error {
	return verifyOwnerOnlyHandle(windows.Handle(file.Fd()), true)
}

func withStoreLock(tokenDir string, fn func() error) error {
	// Directory ACL updates can propagate to a writer's unfinished candidate.
	// Serialize repair and writes before opening the lock file, since creating
	// that file may itself require repairing the directory's write access.
	directory, err := openSecurityHandle(tokenDir, true, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(directory) //nolint:errcheck // lock result is authoritative
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(directory, &info); err != nil {
		return fmt.Errorf("identify provider credential directory: %w", err)
	}
	// Use file identity so case differences and short path aliases share a lock.
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`Global\msgvault-provider-credentials-%08x-%08x%08x`,
		info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow))
	if err != nil {
		return fmt.Errorf("encode provider credential mutex: %w", err)
	}
	mutex, err := windows.CreateMutexEx(nil, name, 0, windows.SYNCHRONIZE|windows.MUTEX_MODIFY_STATE)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("open provider credential mutex: %w", err)
	}
	defer windows.CloseHandle(mutex) //nolint:errcheck // lock result is authoritative
	// Windows mutex ownership belongs to the OS thread, not the goroutine.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	status, err := windows.WaitForSingleObject(mutex, windows.INFINITE)
	if err != nil {
		return fmt.Errorf("wait for provider credential mutex: %w", err)
	}
	if status != windows.WAIT_OBJECT_0 && status != windows.WAIT_ABANDONED {
		return fmt.Errorf("unexpected provider credential mutex wait result: %d", status)
	}
	defer windows.ReleaseMutex(mutex) //nolint:errcheck // lock result is authoritative
	if err := secureOwnerOnlyHandle(directory); err != nil {
		return fmt.Errorf("secure provider credential directory: %w", err)
	}

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
