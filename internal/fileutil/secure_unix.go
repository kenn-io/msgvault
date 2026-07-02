//go:build !windows

// Package fileutil provides cross-platform secure file helpers.
//
// On non-Windows targets, Secure* helpers are thin wrappers over os.* and do
// not add symlink traversal or TOCTOU protections. On Windows, owner-only
// modes additionally set a DACL restricting access to the current user.
package fileutil

import "os"

// SecureWriteFile writes data to the named file, creating it if necessary.
func SecureWriteFile(path string, data []byte, perm os.FileMode) error {
	// codeql[go/path-injection] -- callers provide local user-owned paths;
	// this helper preserves os.WriteFile semantics on non-Windows platforms.
	return os.WriteFile(path, data, perm)
}

// SecureMkdirAll creates a directory path and all parents that do not yet exist.
func SecureMkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

// SecureChmod changes the mode of the named file.
func SecureChmod(path string, perm os.FileMode) error {
	return os.Chmod(path, perm)
}

// SecureOpenFile opens the named file with specified flag and permissions.
func SecureOpenFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag, perm)
}
