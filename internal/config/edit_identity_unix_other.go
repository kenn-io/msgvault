//go:build aix || dragonfly || freebsd || netbsd || openbsd || solaris

package config

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func openedUnixFileIdentity(_ *os.File, info fs.FileInfo) (string, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("unix:%d:%d", stat.Dev, stat.Ino), true
}

func pathEntryIdentity(_ string, info fs.FileInfo) (string, bool) {
	return openedUnixFileIdentity(nil, info)
}
