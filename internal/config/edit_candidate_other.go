//go:build !darwin && !linux && !windows

package config

import (
	"errors"
	"fmt"
	"os"
)

func createConfigCandidate(dir string) (configCandidate, error) {
	file, err := os.CreateTemp(dir, ".config-edit-*.toml.tmp")
	if err != nil {
		return configCandidate{}, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return configCandidate{}, err
	}
	expectedIdentity, ok := openedFileIdentity(file, info)
	if !ok {
		_ = file.Close()
		return configCandidate{}, errors.New("config candidate identity is unavailable")
	}
	retained, err := os.Open(file.Name())
	if err != nil {
		_ = file.Close()
		return configCandidate{}, fmt.Errorf("retain config candidate: %w", err)
	}
	retainedInfo, err := retained.Stat()
	if err != nil {
		_ = retained.Close()
		_ = file.Close()
		return configCandidate{}, err
	}
	retainedIdentity, ok := openedFileIdentity(retained, retainedInfo)
	if !ok || retainedIdentity != expectedIdentity {
		_ = retained.Close()
		_ = file.Close()
		return configCandidate{}, ErrConfigConflict
	}
	return configCandidate{
		file:     file,
		retained: retained,
		path:     file.Name(),
		cleanup: func() error {
			return errors.Join(ErrConfigChanged, ErrConfigConflict,
				errors.New("config retirement is unsupported on this platform; candidate preserved"))
		},
		release: retained.Close,
	}, nil
}
