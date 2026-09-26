package fileutil

import (
	"fmt"
	"os"

	"go.kenn.io/kit/atomicfile"
)

// SecureReplaceFile atomically replaces path with data. The staged file gets
// perm through SecureChmod before any data is written, so an owner-only perm
// also gets the owner-only DACL on Windows. The staged file and its directory
// are fsynced. A symlink or junction at path is refused, not replaced.
//
// An error wrapping atomicfile.ErrPublished means data is already visible at
// path but a later step, such as the directory fsync, failed.
func SecureReplaceFile(path string, data []byte, perm os.FileMode) error {
	// WithPrivate also grants SYSTEM and Administrators access on Windows;
	// retain SecureChmod's current-user-only DACL for owner-only modes.
	file, err := atomicfile.Create(path, atomicfile.WithPerm(perm))
	if err != nil {
		return fmt.Errorf("stage replacement: %w", err)
	}
	defer func() { _ = file.Abort() }()
	if err := SecureChmod(file.TempName(), perm); err != nil {
		return fmt.Errorf("secure replacement: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write replacement: %w", err)
	}
	if err := file.Commit(); err != nil {
		return fmt.Errorf("publish replacement: %w", err)
	}
	return nil
}
