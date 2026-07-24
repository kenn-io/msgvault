//go:build !darwin && !linux && !windows

package config

import "os"

func publishNewConfig(string, *os.File, ConfigFile) (configPublication, error) {
	return configPublication{}, ErrAtomicReplaceUnsupported
}
