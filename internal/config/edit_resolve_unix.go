//go:build darwin || linux

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// resolveOwnedSymlinks walks from a root directory handle and reads symlink
// contents from handles opened without following them. Intermediate pathname
// entries can therefore be renamed without redirecting this traversal.
func resolveOwnedSymlinks(path string, owner func(fs.FileInfo) (uint64, bool)) (string, error) {
	return resolveOwnedSymlinksPinnedFile(path, owner, nil, nil, nil)
}

func resolveOwnedSymlinksPinned(
	path string,
	owner func(fs.FileInfo) (uint64, bool),
	beforeSymlinkRead func(*os.File),
) (string, error) {
	return resolveOwnedSymlinksPinnedFile(path, owner, beforeSymlinkRead, nil, nil)
}

func resolveOwnedSymlinksPinnedFile(
	path string,
	owner func(fs.FileInfo) (uint64, bool),
	beforeSymlinkRead func(*os.File),
	finalEntry func(string, *os.File, fs.FileInfo) error,
	missingParent func(string, *os.File) error,
) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	euid, supported := effectiveUserID()
	if !supported {
		return "", errors.New("symlink ownership verification is unavailable")
	}
	root, err := os.Open(string(filepath.Separator))
	if err != nil {
		return "", err
	}
	stack := []*os.File{root}
	components := make([]string, 0, 16)
	defer func() {
		for _, handle := range stack {
			_ = handle.Close()
		}
	}()
	queue := splitAbsolutePath(absolute)
	for hops := 0; len(queue) > 0; {
		part := queue[0]
		queue = queue[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(stack) > 1 {
				_ = stack[len(stack)-1].Close()
				stack = stack[:len(stack)-1]
				components = components[:len(components)-1]
			}
			continue
		}
		displayPath := filepath.Join(append([]string{string(filepath.Separator)}, append(components, part)...)...)
		entry, info, openErr := openPinnedPathEntry(stack[len(stack)-1], part, displayPath)
		if errors.Is(openErr, fs.ErrNotExist) {
			if missingParent != nil {
				if err := missingParent(displayPath, stack[len(stack)-1]); err != nil {
					return "", err
				}
			}
			return filepath.Join(append([]string{displayPath}, queue...)...), nil
		}
		if openErr != nil {
			return "", openErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > 255 {
				_ = entry.Close()
				return "", errors.New("too many config symlink hops")
			}
			uid, ok := owner(info)
			if !ok || (uid != euid && uid != 0) {
				_ = entry.Close()
				return "", errors.New("symlink hop is not owned by the effective user or root")
			}
			if beforeSymlinkRead != nil {
				beforeSymlinkRead(entry)
			}
			target, readErr := readPinnedSymlink(entry)
			_ = entry.Close()
			if readErr != nil {
				return "", fmt.Errorf("read pinned config symlink %s: %w", displayPath, readErr)
			}
			if filepath.IsAbs(target) {
				for len(stack) > 1 {
					_ = stack[len(stack)-1].Close()
					stack = stack[:len(stack)-1]
				}
				components = components[:0]
			}
			queue = append(splitPathComponents(target), queue...)
			if len(queue) == 0 {
				return "", errors.Join(ErrUnsafeConfigTarget, errors.New("config symlink target is not a regular file"))
			}
			continue
		}
		if len(queue) == 0 {
			if finalEntry != nil {
				readable, readableInfo, openErr := openPinnedReadableFinal(stack[len(stack)-1], part, entry, info, displayPath)
				if openErr != nil {
					_ = entry.Close()
					return "", openErr
				}
				if err := finalEntry(displayPath, readable, readableInfo); err != nil {
					_ = readable.Close()
					_ = entry.Close()
					return "", err
				}
				_ = readable.Close()
			}
			_ = entry.Close()
			return displayPath, nil
		}
		if !info.IsDir() {
			_ = entry.Close()
			return "", fmt.Errorf("config path component %s is not a directory", displayPath)
		}
		stack = append(stack, entry)
		components = append(components, part)
	}
	return filepath.Join(append([]string{string(filepath.Separator)}, components...)...), nil
}

func readConfigFileSnapshot(path string) (ConfigFile, error) {
	return readConfigFileSnapshotUnix(path, false)
}

func readConfigFileSnapshotForEdit(path string) (ConfigFile, error) {
	return readConfigFileSnapshotUnix(path, true)
}

func readConfigFileSnapshotUnix(path string, retain bool) (ConfigFile, error) {
	var snapshot ConfigFile
	requestedInfo, requestedErr := os.Lstat(path)
	requestedSymlink := requestedErr == nil && requestedInfo.Mode()&os.ModeSymlink != 0
	resolved, err := resolveOwnedSymlinksPinnedFile(path, fileOwner, nil,
		func(displayPath string, file *os.File, info fs.FileInfo) error {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%w: config path is not a regular file", ErrUnsafeConfigTarget)
			}
			content, mode, identity, readErr := readVerifiedOpenedConfig(file)
			if readErr != nil {
				return fmt.Errorf("read config file: %w", readErr)
			}
			snapshot = ConfigFile{Path: displayPath, Content: content, ETag: configETag(content), Mode: mode, Exists: true, identity: identity}
			if retain {
				fd, duplicateErr := unix.Dup(int(file.Fd()))
				if duplicateErr != nil {
					return fmt.Errorf("retain config snapshot identity: %w", duplicateErr)
				}
				unix.CloseOnExec(fd)
				snapshot.retained = os.NewFile(uintptr(fd), displayPath)
			}
			return nil
		}, func(displayPath string, parent *os.File) error {
			info, statErr := parent.Stat()
			if statErr != nil {
				return statErr
			}
			identity, ok := openedFileIdentity(parent, info)
			if !ok {
				return errors.Join(ErrUnsafeConfigTarget, errors.New("config parent identity is unavailable"))
			}
			snapshot.Path = displayPath
			snapshot.parentIdentity = identity
			return nil
		})
	if err != nil {
		if snapshot.retained != nil {
			_ = snapshot.retained.Close()
		}
		return ConfigFile{}, fmt.Errorf("%w: resolve config path: %w", ErrUnsafeConfigTarget, err)
	}
	if snapshot.Exists {
		return snapshot, nil
	}
	if requestedSymlink {
		return ConfigFile{}, errors.Join(ErrUnsafeConfigTarget, errors.New("config symlink target does not exist"))
	}
	snapshot.Path = resolved
	snapshot.ETag = configETag(nil)
	snapshot.Mode = 0o600
	return snapshot, nil
}

func splitAbsolutePath(path string) []string {
	volume := filepath.VolumeName(path)
	return splitPathComponents(strings.TrimPrefix(path, volume+string(filepath.Separator)))
}

func splitPathComponents(path string) []string {
	return strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
}
