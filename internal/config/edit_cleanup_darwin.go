//go:build darwin

package config

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func renameConfigNoReplaceAt(dirfd int, oldName, newName string) error {
	if err := unix.RenameatxNp(dirfd, oldName, dirfd, newName, unix.RENAME_EXCL); err != nil {
		return fmt.Errorf("rename config entry without replacement: %w", err)
	}
	return nil
}
