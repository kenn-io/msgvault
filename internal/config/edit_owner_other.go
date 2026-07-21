//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package config

import "io/fs"

func effectiveUserID() (uint64, bool) {
	return 0, false
}

func fileOwner(fs.FileInfo) (uint64, bool) {
	return 0, false
}
