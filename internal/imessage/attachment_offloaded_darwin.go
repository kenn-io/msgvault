package imessage

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func attachmentOffloaded(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Flags&unix.SF_DATALESS != 0
}
