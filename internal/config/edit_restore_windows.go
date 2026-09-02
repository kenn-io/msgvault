//go:build windows

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func retireExactConfigForMissingRestore(current, before ConfigFile) error {
	authority, err := pinWindowsConfigParent(before.Path)
	if err != nil {
		return err
	}
	defer func() { _ = authority.Release() }()
	parent, err := os.Open(filepath.Dir(before.Path))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	info, err := parent.Stat()
	if err != nil {
		return err
	}
	identity, ok := openedFileIdentity(parent, info)
	if !ok || identity != before.parentIdentity {
		return errors.Join(ErrConfigConflict, errors.New("config rollback parent changed"))
	}
	// Retain by identity, not by name: reopening current.Path without
	// comparing the opened file against the pinned current.identity would let
	// a byte-identical replacement substituted between the initial read and
	// this point be quarantined as if it were the published config. The
	// attribute-only retention also never conflicts with the DELETE-access
	// opens the quarantine rename performs.
	retained, err := retainWindowsConfigArtifact(current.Path, current.identity)
	if err != nil {
		return err
	}
	defer func() { _ = retained.Close() }()
	if err := retireWindowsConfigArtifact(current.Path, retained); err != nil {
		return fmt.Errorf("retire created config during rollback: %w", err)
	}
	return nil
}
