//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package config

import (
	"io/fs"
	"os"
	"syscall"
)

func effectiveUserID() (uint64, bool) {
	uid := os.Geteuid()
	if uid < 0 {
		return 0, false
	}
	return uint64(uid), true
}

func fileOwner(info fs.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Uid), true
}
