package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"time"
)

func run() error {
	files, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		return fmt.Errorf("list tracked files: %w", err)
	}
	for name := range bytes.SplitSeq(files, []byte{0}) {
		if len(name) == 0 {
			continue
		}
		path := string(name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		hash := sha256.Sum256(data)
		offset := binary.BigEndian.Uint64(hash[24:]) & ((1 << 48) - 1)
		// NTFS preserves 100 ns steps; the full range stays within 2000.
		stamp := time.Unix(946684800, int64(offset)*100)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			return fmt.Errorf("set times for %s: %w", path, err)
		}
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
