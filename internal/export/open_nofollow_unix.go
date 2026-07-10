//go:build unix

package export

import (
	"os"

	"golang.org/x/sys/unix"
)

// openNoFollow opens a file read-only without following symlinks.
// Uses O_NOFOLLOW to prevent symlink traversal on the final path component.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
}

// openNoFollowDurable opens the final component read-write so Sync has the
// strongest portable semantics while retaining O_NOFOLLOW.
func openNoFollowDurable(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|unix.O_NOFOLLOW, 0)
}

func validateNoFollowFileInfo(os.FileInfo) error { return nil }
