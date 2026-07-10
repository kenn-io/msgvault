//go:build !windows && !unix

package export

import "os"

// openNoFollow opens a file read-only.
// On platforms without Unix or Windows support, this is a fallback that does
// not enforce no-follow semantics. Callers validate size and content hash.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY, 0)
}

func openNoFollowDurable(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}

func validateNoFollowFileInfo(os.FileInfo) error { return nil }
