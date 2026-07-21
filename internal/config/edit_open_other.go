//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package config

import (
	"fmt"
	"io/fs"
	"os"
)

func openConfigNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}

func openedFileIdentity(_ *os.File, info fs.FileInfo) (string, bool) {
	// These platforms cannot perform conditional replacement, but stable
	// metadata still lets read-only settings snapshots function.
	return fmt.Sprintf("generic:%d:%d:%d", info.Size(), info.ModTime().UnixNano(), info.Mode()), true
}

func pathEntryIdentity(_ string, info fs.FileInfo) (string, bool) {
	return openedFileIdentity(nil, info)
}
