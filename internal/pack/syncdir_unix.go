//go:build !windows

package pack

import (
	"fmt"
	"os"
)

// syncDirPlatform fsyncs a directory so a recently changed entry inside it
// (a rename, a new file, a new subdirectory) survives a crash. POSIX allows
// opening a directory read-only and syncing the resulting descriptor.
func syncDirPlatform(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("pack: opening directory for sync: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("pack: syncing directory: %w", err)
	}
	return nil
}
