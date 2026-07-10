//go:build unix

package export

import "os"

func snapshotAttachmentPathIdentity(path string) (os.FileInfo, error) {
	return os.Lstat(path)
}
