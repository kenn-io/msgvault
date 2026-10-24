//go:build linux

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// Tests replace these to simulate filesystems that do not report birth time
// or file handles.
var (
	linuxStatx          = unix.Statx
	linuxNameToHandleAt = unix.NameToHandleAt
)

func openedUnixFileIdentity(file *os.File, _ fs.FileInfo) (string, bool) {
	return linuxFileIdentity(int(file.Fd()), "", unix.AT_EMPTY_PATH)
}

func pathEntryIdentity(path string, _ fs.FileInfo) (string, bool) {
	return linuxFileIdentity(unix.AT_FDCWD, path, 0)
}

// linuxFileIdentity names the file at dirfd and path without following a
// final symlink. AT_EMPTY_PATH in flags names dirfd itself.
func linuxFileIdentity(dirfd int, path string, flags int) (string, bool) {
	var stat unix.Statx_t
	if err := linuxStatx(
		dirfd,
		path,
		flags|unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_SYNC_AS_STAT,
		unix.STATX_INO|unix.STATX_BTIME,
		&stat,
	); err != nil || stat.Mask&unix.STATX_INO == 0 {
		return "", false
	}
	if stat.Mask&unix.STATX_BTIME != 0 {
		return fmt.Sprintf(
			"linux:%d:%d:%d:%d:%d",
			stat.Dev_major,
			stat.Dev_minor,
			stat.Ino,
			stat.Btime.Sec,
			stat.Btime.Nsec,
		), true
	}
	// Some filesystems do not record birth time, such as ext4 created with
	// 128-byte inodes. Prefer the file handle there. It is opaque, and on
	// ext4 it carries the inode generation, which changes when an inode
	// number is reused. name_to_handle_at never follows a final symlink
	// without AT_SYMLINK_FOLLOW.
	handle, _, err := linuxNameToHandleAt(dirfd, path, flags&unix.AT_EMPTY_PATH)
	switch {
	case err == nil:
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EPERM):
		// The filesystem cannot encode handles, the kernel lacks them, or a
		// seccomp policy blocks the call. Fall back to device and inode, as
		// on the BSDs. That identity cannot detect a reused inode number
		// once the earlier descriptor is closed, such as between a public
		// snapshot and a later edit. Comparisons made while a pinned
		// descriptor is held are unaffected, because the inode cannot be
		// freed while it is open.
		return fmt.Sprintf("linux-ino:%d:%d:%d", stat.Dev_major, stat.Dev_minor, stat.Ino), true
	default:
		return "", false
	}
	return fmt.Sprintf(
		"linux-handle:%d:%d:%d:%d:%x",
		stat.Dev_major,
		stat.Dev_minor,
		stat.Ino,
		handle.Type(),
		handle.Bytes(),
	), true
}
