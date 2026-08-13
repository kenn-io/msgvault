//go:build !windows

package mistral

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func availableDiskBytes(path string) (int64, error) {
	var status unix.Statfs_t
	if err := unix.Statfs(path, &status); err != nil {
		return 0, fmt.Errorf("stat filesystem: %w", err)
	}
	if status.Bavail <= 0 || status.Bsize <= 0 {
		return 0, nil
	}
	blocks := status.Bavail
	blockSize := uint64(status.Bsize)
	if blockSize != 0 && blocks > math.MaxUint64/blockSize {
		return math.MaxInt64, nil
	}
	available := blocks * blockSize
	if available > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(available), nil
}
