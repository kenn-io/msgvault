//go:build windows

package tasklinks

import (
	"os"
)

func cachePersistencePolicy() error {
	return ErrDiskCacheSecurityUnsupported
}

func secureCacheDirectory(string) error {
	return ErrDiskCacheSecurityUnsupported
}

func secureCacheFile(*os.File) error {
	return ErrDiskCacheSecurityUnsupported
}

func validateCacheFile(string, os.FileInfo) error {
	return ErrDiskCacheSecurityUnsupported
}
