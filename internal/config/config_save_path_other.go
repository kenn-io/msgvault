//go:build !windows

package config

import (
	"os"
	"path/filepath"
)

func prepareConfigSavePath(path string) (string, func() error, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil, nil
	}
	if target, err := os.Readlink(path); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return target, nil, nil
	}
	return path, nil, nil
}
