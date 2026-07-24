//go:build windows

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func resolveOwnedSymlinks(path string, _ func(fs.FileInfo) (uint64, bool)) (string, error) {
	if err := rejectWindowsReparseComponents(path); err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(path)
	return filepath.Clean(absolute), err
}

func resolveOwnedSymlinksPinned(path string, owner func(fs.FileInfo) (uint64, bool), _ func(*os.File)) (string, error) {
	return resolveOwnedSymlinks(path, owner)
}

func readConfigFileSnapshot(path string) (ConfigFile, error) {
	authority, err := pinWindowsConfigParent(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return ConfigFile{}, err
		}
		var resolved, ancestorIdentity string
		authority, resolved, ancestorIdentity, err = pinWindowsNearestExistingConfigAncestor(path)
		if err != nil {
			return ConfigFile{}, err
		}
		defer func() { _ = authority.Release() }()
		return ConfigFile{
			Path: resolved, ETag: configETag(nil), Mode: 0o600,
			parentIdentity: ancestorIdentity,
		}, nil
	}
	defer func() { _ = authority.Release() }()
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
	} else {
		parent, openErr := os.Open(filepath.Dir(resolved))
		if openErr != nil {
			return ConfigFile{}, openErr
		}
		info, statErr := parent.Stat()
		if statErr != nil {
			_ = parent.Close()
			return ConfigFile{}, statErr
		}
		identity, ok := openedFileIdentity(parent, info)
		_ = parent.Close()
		if !ok {
			return ConfigFile{}, errors.Join(ErrUnsafeConfigTarget, errors.New("config parent identity is unavailable"))
		}
		return ConfigFile{Path: resolved, ETag: configETag(nil), Mode: mode, parentIdentity: identity}, nil
	}
	return ConfigFile{Path: resolved, Content: content, ETag: configETag(content), Mode: mode, Exists: exists, identity: identity}, nil
}

func readConfigFileSnapshotForEdit(path string) (ConfigFile, error) {
	return readConfigFileSnapshot(path)
}
