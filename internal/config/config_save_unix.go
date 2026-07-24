//go:build !windows

package config

import "os"

type configSaveHooks struct{}

func publishSavedConfig(candidatePath, targetPath string, _ *os.File, _ configSaveHooks) (bool, error) {
	if err := os.Rename(candidatePath, targetPath); err != nil {
		return false, err
	}
	return true, nil
}
