//go:build !windows

package config

import (
	"io/fs"
	"os"
)

func secureConfigCandidate(file *os.File, _ string, mode fs.FileMode) error {
	return file.Chmod(mode)
}

func validateOpenedConfigSecurity(*os.File) error { return nil }

func openConfigDirectoryForSync(path string) (syncDirectoryHandle, error) {
	return os.Open(path)
}
