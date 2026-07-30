package query

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"go.kenn.io/kit/daemon"
)

var daemonSpillNamePattern = regexp.MustCompile(`^duckdb-query-([1-9][0-9]*)$`)

// PrepareDaemonSpillDir removes validated stale daemon spill directories and
// returns the current process's owned directory.
func PrepareDaemonSpillDir(home string) (string, error) {
	return prepareDaemonSpillDir(home)
}

func prepareDaemonSpillDir(home string) (string, error) {
	if home == "" {
		return "", errors.New("prepare daemon DuckDB spill: empty home directory")
	}
	absoluteHome, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("prepare daemon DuckDB spill home: %w", err)
	}
	tmpRoot := filepath.Join(absoluteHome, "tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return "", fmt.Errorf("create daemon DuckDB spill root: %w", err)
	}

	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		return "", fmt.Errorf("read daemon DuckDB spill root: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		match := daemonSpillNamePattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		pid, err := strconv.Atoi(match[1])
		if err != nil || pid <= 0 || daemon.ProcessAlive(pid) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(tmpRoot, entry.Name())); err != nil {
			return "", fmt.Errorf("remove stale daemon DuckDB spill %s: %w", entry.Name(), err)
		}
	}

	owned := filepath.Join(tmpRoot, fmt.Sprintf("duckdb-query-%d", os.Getpid()))
	if err := os.MkdirAll(owned, 0o700); err != nil {
		return "", fmt.Errorf("create owned daemon DuckDB spill: %w", err)
	}
	return owned, nil
}
