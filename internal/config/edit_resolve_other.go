//go:build !darwin && !linux && !windows

package config

import (
	"fmt"
	"io/fs"
	"os"
)

func resolveOwnedSymlinks(path string, owner func(fs.FileInfo) (uint64, bool)) (string, error) {
	return resolveOwnedSymlinksWithReadlink(path, owner, os.Readlink)
}

func resolveOwnedSymlinksPinned(path string, owner func(fs.FileInfo) (uint64, bool), _ func(*os.File)) (string, error) {
	return resolveOwnedSymlinks(path, owner)
}

func readConfigFileSnapshot(path string) (ConfigFile, error) {
	resolved, mode, exists, err := resolveConfigTargetWithOwner(path, fileOwner)
	if err != nil {
		return ConfigFile{}, err
	}
	var content []byte
	var identity string
	if exists {
		content, mode, identity, err = readVerifiedPhysicalConfig(resolved)
		if err != nil {
			return ConfigFile{}, fmt.Errorf("read config file: %w", err)
		}
	}
	return ConfigFile{Path: resolved, Content: content, ETag: configETag(content), Mode: mode, Exists: exists, identity: identity}, nil
}

func readConfigFileSnapshotForEdit(path string) (ConfigFile, error) {
	return readConfigFileSnapshot(path)
}
