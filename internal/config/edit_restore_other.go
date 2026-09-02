//go:build !darwin && !linux && !windows

package config

import "errors"

func retireExactConfigForMissingRestore(ConfigFile, ConfigFile) error {
	return errors.Join(ErrAtomicReplaceUnsupported, errors.New("missing config rollback is unsupported on this platform"))
}
