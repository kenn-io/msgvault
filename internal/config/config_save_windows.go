//go:build windows

package config

import (
	"errors"
	"fmt"
	"os"
)

type configSaveHooks struct {
	beforeExistingRetirement func(configReplacement) error
}

func publishSavedConfig(
	candidatePath, targetPath string,
	retained *os.File,
	hooks configSaveHooks,
) (bool, error) {
	candidate, candidateErr := readPhysicalConfigSnapshot(candidatePath)
	if candidateErr != nil {
		return false, candidateErr
	}
	target, targetErr := readConfigFileSnapshot(targetPath)
	if targetErr == nil && !target.Exists {
		publication, err := publishNewConfig(candidatePath, retained, target)
		if publication.release != nil {
			_ = publication.release()
		}
		return err == nil, err
	}
	if targetErr != nil {
		return false, targetErr
	}
	replacement, err := beginConfigReplacement(candidatePath, targetPath, candidate, target)
	if replacement.release != nil {
		defer func() { _ = replacement.release() }()
	}
	if err != nil {
		return replacement.preserveCandidate, err
	}
	if hooks.beforeExistingRetirement != nil {
		if err := hooks.beforeExistingRetirement(replacement); err != nil {
			return true, errors.Join(ErrConfigChanged,
				fmt.Errorf("before displaced config retirement: %w", err))
		}
	}
	if replacement.cleanupDisplaced == nil {
		return true, errors.Join(ErrConfigChanged, errors.New("config replacement has no retirement operation"))
	}
	if err := replacement.cleanupDisplaced(); err != nil {
		return true, errors.Join(ErrConfigChanged, err, recoveryArtifactError(replacement))
	}
	return true, nil
}
