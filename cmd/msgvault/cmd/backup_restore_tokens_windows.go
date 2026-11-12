//go:build windows

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func secureRestoredTokens(ctx context.Context, repo *backup.Repo, snapshotID, target string, extrasFiles int) error {
	if extrasFiles == 0 {
		return nil
	}
	dir := filepath.Join(target, "tokens")
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect restored token directory: %w", err)
	}
	if !info.IsDir() {
		return nil
	}
	snapshot, err := repo.OpenSnapshot(ctx, snapshotID, backupapp.New(Version).PackFileExtension())
	if err != nil {
		return fmt.Errorf("open restored snapshot: %w", err)
	}
	defer snapshot.Close() //nolint:errcheck // membership lookup is authoritative
	strict := false
	var nonRegular string
	var selected []string
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir || entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		_, err = snapshot.Extra("tokens/" + filepath.ToSlash(rel))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect restored token membership: %w", err)
		}
		if !entry.Type().IsRegular() && nonRegular == "" {
			nonRegular = rel
		}
		if strings.EqualFold(rel, providercredentials.Filename) || strings.EqualFold(rel, providercredentials.ServerKeyFilename) {
			strict = true
		}
		selected = append(selected, rel)
		return nil
	})
	if err != nil {
		return fmt.Errorf("inspect restored tokens: %w", err)
	}
	if !strict {
		return nil
	}
	if nonRegular != "" {
		return fmt.Errorf("inspect restored tokens: restored token %q is not a regular file", nonRegular)
	}
	err = providercredentials.SecureRestored(dir, selected)
	if errors.Is(err, providercredentials.ErrRestorePermissionsConflict) {
		return fmt.Errorf("%w; credential permissions were not changed", err)
	}
	if err != nil {
		return fmt.Errorf("%w; credentials may be unreadable and permissions may have changed", err)
	}
	return nil
}
