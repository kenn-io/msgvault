//go:build windows

package pack

// syncDirPlatform is a no-op on Windows: unlike POSIX, Windows does not let
// a caller open a directory and fsync the resulting handle to force
// directory-entry changes (renames, new files, new subdirectories) to disk.
// Durability of pack contents themselves still comes from fsyncing the pack
// file before it is renamed into place; only the directory-entry durability
// this primitive would add is unavailable here.
func syncDirPlatform(dir string) error {
	return nil
}
