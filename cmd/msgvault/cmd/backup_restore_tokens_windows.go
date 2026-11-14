//go:build windows

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func secureRestoredTokens(ctx context.Context, repo *backup.Repo, snapshotID, target string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	lease, err := repo.AcquireSharedLock("secure restored tokens", false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	manifest, err := repo.LoadManifest(snapshotID)
	if err != nil {
		return err
	}
	if !manifest.Options.IncludeTokens || manifest.Extras.Tree == "" {
		return nil
	}
	id, err := pack.ParseBlobID(manifest.Extras.Tree)
	if err != nil {
		return err
	}
	index, err := repo.LoadBlobIndex()
	if err != nil {
		return err
	}
	raw, err := repo.ReadBlob(index, id, nil, backupapp.New(Version).PackFileExtension())
	if err != nil {
		return err
	}
	var tree backup.ExtrasTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		return fmt.Errorf("read snapshot extras: %w", err)
	}
	var selected []string
	for _, entry := range tree.Entries {
		prefix, name, ok := strings.Cut(entry.Path, "/")
		if !ok || !strings.EqualFold(prefix, "tokens") {
			continue
		}
		if !fs.ValidPath(name) || strings.Contains(name, `\`) || !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("invalid snapshot token path %q", entry.Path)
		}
		selected = append(selected, filepath.FromSlash(name))
	}
	if len(selected) == 0 {
		return nil
	}
	err = providercredentials.SecureRestored(filepath.Join(target, "tokens"), selected)
	if errors.Is(err, providercredentials.ErrRestorePermissionsConflict) {
		return fmt.Errorf("%w; credential permissions were not changed; resolve the conflict or move the omitted entry aside", err)
	}
	if err != nil {
		return fmt.Errorf("%w; credentials may be unreadable and permissions may have changed", err)
	}
	return nil
}
