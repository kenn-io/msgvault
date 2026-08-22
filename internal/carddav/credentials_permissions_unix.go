//go:build !windows

package carddav

import (
	"errors"
	"os"
)

type nativeCredentialPermissions struct{}

func (nativeCredentialPermissions) secureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700) // #nosec G302 -- path is a directory and 0700 is the required private mode.
}

func (nativeCredentialPermissions) secureFile(file *os.File) error {
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return nativeCredentialPermissions{}.verifyFile(file)
}

func (nativeCredentialPermissions) verifyFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o600 {
		return errors.New("CardDAV token file permissions must be 0600")
	}
	return nil
}
