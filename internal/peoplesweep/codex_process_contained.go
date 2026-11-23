//go:build linux || darwin

package peoplesweep

import (
	"os"
	"syscall"
)

func codexAuthOwnedByDaemon(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	uid := os.Geteuid()
	return ok && uid >= 0 && uint64(stat.Uid) == uint64(uid)
}
