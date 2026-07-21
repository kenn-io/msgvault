//go:build windows

package config

import (
	"errors"
	"os"
	"path/filepath"
)

func prepareConfigSavePath(path string) (string, func() error, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	authority, err := pinWindowsConfigParent(absolute)
	if err != nil {
		return "", nil, err
	}
	info, statErr := os.Lstat(absolute)
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		_ = authority.Release()
		return "", nil, errors.Join(ErrUnsafeConfigTarget, errors.New("Windows config path is a reparse point"))
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		_ = authority.Release()
		return "", nil, statErr
	}
	return filepath.Clean(absolute), authority.Release, nil
}
