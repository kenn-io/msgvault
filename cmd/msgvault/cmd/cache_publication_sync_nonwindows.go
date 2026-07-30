//go:build !windows

package cmd

import "os"

func syncFile(path string) error {
	// #nosec G703 -- callers pass fixed files inside private transaction/cache roots.
	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return file.Sync()
}

func syncDirectory(path string) error {
	// #nosec G703 -- callers pass configured cache parents or validated transaction paths.
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
