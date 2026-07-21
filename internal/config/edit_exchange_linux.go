//go:build linux

package config

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func beginConfigReplacement(left, right string, expectedLeft, expectedRight ConfigFile) (configReplacement, error) {
	if err := verifyRetainedConfigSnapshot(expectedRight); err != nil {
		return configReplacement{}, err
	}
	dir, leftName, rightName, leftBefore, rightBefore, err := openPinnedReplacementDirectory(left, right)
	if err != nil {
		return configReplacement{}, err
	}
	if !sameConfigVersion(leftBefore, expectedLeft) || !sameConfigVersion(rightBefore, expectedRight) {
		_ = unix.Close(dir)
		return configReplacement{}, errors.Join(ErrConfigConflict,
			errors.New("config exchange identities changed before publication"))
	}
	displaced, err := retainConfigEntryAt(dir, rightName, right, rightBefore.identity)
	if err != nil {
		_ = unix.Close(dir)
		return configReplacement{}, fmt.Errorf("retain displaced config identity: %w", err)
	}
	if err := unix.Renameat2(dir, leftName, dir, rightName, unix.RENAME_EXCHANGE); err != nil {
		_ = displaced.Close()
		_ = unix.Close(dir)
		return configReplacement{}, fmt.Errorf("rename exchange: %w", err)
	}
	return configReplacement{
		displacedPath: left,
		rollbackPublished: func(expected ConfigFile) error {
			displaced, displacedErr := readPhysicalConfigSnapshotAt(dir, leftName, left)
			current, currentErr := readPhysicalConfigSnapshotAt(dir, rightName, right)
			if displacedErr != nil || currentErr != nil {
				return errors.Join(ErrConfigChanged, ErrConfigConflict, displacedErr, currentErr)
			}
			if !sameConfigVersion(displaced, rightBefore) {
				return errors.Join(ErrConfigChanged, ErrConfigConflict,
					errors.New("refusing rollback because the displaced config changed"))
			}
			if !sameConfigVersion(current, expected) {
				return errors.Join(ErrConfigChanged, ErrConfigConflict,
					errors.New("refusing rollback because the published config changed"))
			}
			if err := unix.Renameat2(dir, leftName, dir, rightName, unix.RENAME_EXCHANGE); err != nil {
				return fmt.Errorf("rollback rename exchange: %w", err)
			}
			return nil
		},
		published:     leftBefore,
		syncDirectory: func() error { return unix.Fsync(dir) },
		cleanupDisplaced: func() error {
			return retireConfigArtifactAt(dir, leftName, left, displaced)
		},
		release: func() error { return errors.Join(displaced.Close(), unix.Close(dir)) },
	}, nil
}
