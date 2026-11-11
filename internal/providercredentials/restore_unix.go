//go:build !windows

package providercredentials

import (
	"errors"
	"os"
	"path/filepath"
)

func restorePreflight(dir string, _ map[string]struct{}) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("restored token directory must be a real directory")
	}
	return verifyCurrentOwner(info)
}

func secureRestoredFile(path string) error {
	file, err := openSecretFile(path)
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck // read-only handle
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("restored token must be a regular file")
	}
	if err := verifyCurrentOwner(info); err != nil {
		return err
	}
	if (filepath.Base(path) != Filename && verifySecretFile(file) == nil) || (filepath.Base(path) == Filename && (nativePermissions{}).verifyFile(file) == nil) {
		return nil
	}
	if info.Mode().Perm()&0600 != 0600 {
		return errors.New("restored token cannot be secured without widening owner access")
	}
	return (nativePermissions{}).secureFile(file)
}
