//go:build darwin || linux

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openPinnedReplacementDirectory proves both names through one pinned
// directory handle. Native exchange then uses that dirfd, so replacing an
// ancestor pathname cannot redirect the transaction to another file.
func openPinnedReplacementDirectory(left, right string) (int, string, string, ConfigFile, ConfigFile, error) {
	if filepath.Clean(filepath.Dir(left)) != filepath.Clean(filepath.Dir(right)) {
		return -1, "", "", ConfigFile{}, ConfigFile{}, errors.New("config exchange paths are not siblings")
	}
	dir, err := unix.Open(filepath.Dir(right), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", "", ConfigFile{}, ConfigFile{}, fmt.Errorf("pin config directory: %w", err)
	}
	leftName, rightName := filepath.Base(left), filepath.Base(right)
	leftSnapshot, leftErr := readPhysicalConfigSnapshotAt(dir, leftName, left)
	rightSnapshot, rightErr := readPhysicalConfigSnapshotAt(dir, rightName, right)
	if leftErr != nil || rightErr != nil {
		_ = unix.Close(dir)
		return -1, "", "", ConfigFile{}, ConfigFile{}, errors.Join(leftErr, rightErr)
	}
	// Cross-check the path snapshots. If an ancestor changed while the pinned
	// directory was opened, the handle-relative and pathname identities differ.
	pathLeft, pathLeftErr := readPhysicalConfigSnapshot(left)
	pathRight, pathRightErr := readPhysicalConfigSnapshot(right)
	if pathLeftErr != nil || pathRightErr != nil ||
		!sameConfigVersion(leftSnapshot, pathLeft) || !sameConfigVersion(rightSnapshot, pathRight) {
		_ = unix.Close(dir)
		return -1, "", "", ConfigFile{}, ConfigFile{}, errors.Join(
			ErrConfigConflict, errors.New("config directory changed while being pinned"),
			pathLeftErr, pathRightErr,
		)
	}
	return dir, leftName, rightName, leftSnapshot, rightSnapshot, nil
}

func verifyRetainedConfigSnapshot(snapshot ConfigFile) error {
	if snapshot.retained == nil {
		return errors.Join(ErrConfigConflict, errors.New("existing config snapshot identity is not retained"))
	}
	info, err := snapshot.retained.Stat()
	if err != nil {
		return fmt.Errorf("identify retained existing config snapshot: %w", err)
	}
	identity, ok := openedFileIdentity(snapshot.retained, info)
	if !ok || identity != snapshot.identity {
		return errors.Join(ErrConfigConflict, errors.New("retained existing config snapshot identity changed"))
	}
	return nil
}

func readPhysicalConfigSnapshotAt(dir int, name, displayPath string) (ConfigFile, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return ConfigFile{}, fmt.Errorf("open pinned config entry: %w", err)
	}
	file := os.NewFile(uintptr(fd), displayPath)
	content, mode, identity, readErr := readVerifiedOpenedConfig(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return ConfigFile{}, errors.Join(readErr, closeErr)
	}
	return ConfigFile{Path: displayPath, Content: content, ETag: configETag(content), Mode: mode, Exists: true, identity: identity}, nil
}
