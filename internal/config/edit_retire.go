package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// configRetiredPrefix marks deliberately retained, content-preserving recovery
// artifacts. Retirement leaves at most one artifact per transaction object
// instead of deleting a pathname or truncating an inode that may have external
// hardlinks. A future operator-facing maintenance action may manage these;
// transaction code must not mutate them.
const configRetiredPrefix = ".config-retired-"

func newConfigRetiredName() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate config retirement name: %w", err)
	}
	return configRetiredPrefix + hex.EncodeToString(random[:]) + ".tombstone", nil
}
