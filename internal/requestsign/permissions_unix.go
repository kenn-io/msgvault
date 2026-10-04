//go:build unix

package requestsign

import (
	"errors"
	"os"
	"syscall"
)

func privateFilePermissions(_ *os.File, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	unsafeMode := os.FileMode(0o077)
	if info.IsDir() {
		unsafeMode = 0o022
	}
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) || info.Mode().Perm()&unsafeMode != 0 {
		return errors.New("private file must be owned by the current user with no group or other permissions")
	}
	return nil
}
