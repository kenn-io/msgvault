//go:build windows

package export

import (
	"errors"
	"fmt"
	"os"
)

// snapshotAttachmentPathIdentity returns handle-backed metadata for the final
// path component. The shared no-follow handle makes the file ID eager without
// conflicting with concurrent validation or durable handles.
func snapshotAttachmentPathIdentity(path string) (
	resultInfo os.FileInfo, resultErr error,
) {
	f, err := openNoFollowWindows(path, 0)
	if err != nil {
		return nil, fmt.Errorf("open attachment path identity snapshot: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			resultInfo = nil
			resultErr = errors.Join(
				resultErr,
				fmt.Errorf("close attachment path identity snapshot: %w", err),
			)
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat attachment path identity snapshot: %w", err)
	}
	return info, nil
}
