//go:build sqlite_vec && !linux && !darwin

package main

import "os"

func childMaxRSS(_ *os.ProcessState) (*int64, string) {
	return nil, "unavailable"
}
