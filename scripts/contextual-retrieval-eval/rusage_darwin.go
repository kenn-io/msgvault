//go:build sqlite_vec && darwin

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
	bytes := usage.Maxrss
	return &bytes, "darwin_child_maxrss"
}
