//go:build linux

package emlx

import (
	"fmt"
	"os"
	"syscall"
)

func fileIdentity(fi os.FileInfo) (string, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d:%d", st.Dev, st.Ino, st.Ctim.Sec, st.Ctim.Nsec), true
}
