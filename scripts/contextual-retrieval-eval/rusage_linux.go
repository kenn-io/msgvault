//go:build sqlite_vec && linux

package main

import (
	"os"
	"syscall"
)

func childMaxRSS(state *os.ProcessState) (*int64, string) {
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || usage.Maxrss <= 0 {
		return nil, "unavailable"
	}
	bytes := usage.Maxrss * 1024
	return &bytes, "linux_wait4_child_maxrss"
}
