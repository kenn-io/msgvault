//go:build !windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// ensureConfigParentDirectories resolves and validates every existing path
// component before creating the missing directory suffix. The config file is
// still snapshotted after this step, so a concurrent file creation is observed
// by the normal ETag check.
func ensureConfigParentDirectories(path string, _ ...string) error {
	resolved, err := resolveOwnedSymlinks(path, fileOwner)
	if err != nil {
		return fmt.Errorf("resolve config path before creating directories: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return nil
}
