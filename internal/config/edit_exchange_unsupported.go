//go:build !darwin && !linux && !windows

package config

func beginConfigReplacement(_, _ string, _, _ ConfigFile) (configReplacement, error) {
	return configReplacement{}, ErrAtomicReplaceUnsupported
}
