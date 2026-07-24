//go:build linux

package config

import (
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func openedUnixFileIdentity(file *os.File, _ fs.FileInfo) (string, bool) {
	var stat unix.Statx_t
	if err := unix.Statx(
		int(file.Fd()),
		"",
		unix.AT_EMPTY_PATH|unix.AT_STATX_SYNC_AS_STAT,
		unix.STATX_INO|unix.STATX_BTIME,
		&stat,
	); err != nil || stat.Mask&unix.STATX_INO == 0 || stat.Mask&unix.STATX_BTIME == 0 {
		return "", false
	}
	return formatLinuxFileIdentity(&stat), true
}

func pathEntryIdentity(path string, _ fs.FileInfo) (string, bool) {
	var stat unix.Statx_t
	if err := unix.Statx(
		unix.AT_FDCWD,
		path,
		unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_SYNC_AS_STAT,
		unix.STATX_INO|unix.STATX_BTIME,
		&stat,
	); err != nil || stat.Mask&unix.STATX_INO == 0 || stat.Mask&unix.STATX_BTIME == 0 {
		return "", false
	}
	return formatLinuxFileIdentity(&stat), true
}

func formatLinuxFileIdentity(stat *unix.Statx_t) string {
	return fmt.Sprintf(
		"linux:%d:%d:%d:%d:%d",
		stat.Dev_major,
		stat.Dev_minor,
		stat.Ino,
		stat.Btime.Sec,
		stat.Btime.Nsec,
	)
}
