//go:build windows

package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createConfigCandidate applies the protected owner-only DACL in the
// SECURITY_ATTRIBUTES passed to CREATE_NEW. Config bytes are therefore never
// exposed through an inherited directory DACL, even briefly.
func createConfigCandidate(dir string) (configCandidate, error) {
	authority, err := pinWindowsConfigParent(filepath.Join(dir, "config-candidate"))
	if err != nil {
		return configCandidate{}, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		_ = authority.Release()
		return configCandidate{}, fmt.Errorf("get current user SID: %w", err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + user.User.Sid.String() + "D:P(A;;GA;;;" + user.User.Sid.String() + ")",
	)
	if err != nil {
		_ = authority.Release()
		return configCandidate{}, fmt.Errorf("build config candidate security descriptor: %w", err)
	}
	attributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	for range 128 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			_ = authority.Release()
			return configCandidate{}, err
		}
		path := filepath.Join(dir, ".config-edit-"+hex.EncodeToString(random[:])+".toml.tmp")
		path16, err := windows.UTF16PtrFromString(path)
		if err != nil {
			_ = authority.Release()
			return configCandidate{}, err
		}
		handle, err := windows.CreateFile(
			path16,
			windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE,
			attributes,
			windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err != nil {
			if err == windows.ERROR_FILE_EXISTS || err == windows.ERROR_ALREADY_EXISTS {
				continue
			}
			_ = authority.Release()
			return configCandidate{}, err
		}
		file := os.NewFile(uintptr(handle), path)
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			_ = authority.Release()
			return configCandidate{}, statErr
		}
		identity, ok := openedFileIdentity(file, info)
		if !ok {
			_ = file.Close()
			_ = authority.Release()
			return configCandidate{}, errors.New("config candidate identity is unavailable")
		}
		// A fresh attributes-only open replaces a DuplicateHandle retention:
		// a duplicated handle would keep the writable open's sharing footprint
		// on the file object alive after the write handle closes, which would
		// make ReplaceFileW's no-sharing open of the candidate fail. Matching
		// the identity against the still-open create handle proves the
		// retained descriptor names the same file.
		retained, err := retainWindowsConfigArtifact(path, identity)
		if err != nil {
			_ = file.Close()
			_ = authority.Release()
			return configCandidate{}, fmt.Errorf("retain Windows config candidate: %w", err)
		}
		return configCandidate{
			file:     file,
			retained: retained,
			path:     path,
			release: func() error {
				return errors.Join(retained.Close(), authority.Release())
			},
			cleanup: func() error {
				return retireWindowsConfigArtifact(path, retained)
			},
		}, nil
	}
	_ = authority.Release()
	return configCandidate{}, errors.New("could not allocate a unique config candidate")
}
