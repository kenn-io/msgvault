//go:build windows

package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func secureRestoredTokens(ctx context.Context, repo *backup.Repo, snapshotID, target string, forceUnlock bool) (count int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	lease, err := repo.AcquireSharedLock("secure restored tokens", forceUnlock)
	if err != nil {
		return 0, fmt.Errorf("lock snapshot repository: %w", err)
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	manifest, err := repo.LoadManifest(snapshotID)
	if err != nil {
		return 0, fmt.Errorf("load snapshot manifest: %w", err)
	}
	if manifest.Extras.Tree == "" {
		return 0, nil
	}
	id, err := pack.ParseBlobID(manifest.Extras.Tree)
	if err != nil {
		return 0, fmt.Errorf("read saved credentials: %w", err)
	}
	index, err := repo.LoadBlobIndex()
	if err != nil {
		return 0, fmt.Errorf("read saved credentials: %w", err)
	}
	raw, err := repo.ReadBlob(index, id, nil, backupapp.New(Version).PackFileExtension())
	if err != nil {
		return 0, fmt.Errorf("read saved credentials: %w", err)
	}
	var tree backup.ExtrasTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		return 0, fmt.Errorf("read saved credentials: %w", err)
	}
	var selected []string
	for _, entry := range tree.Entries {
		prefix, name, ok := strings.Cut(entry.Path, "/")
		if !ok || !strings.EqualFold(prefix, "tokens") {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return 0, fmt.Errorf("invalid snapshot token path %q", entry.Path)
		}
		selected = append(selected, filepath.FromSlash(name))
	}
	if len(selected) == 0 {
		return 0, nil
	}
	err = providercredentials.SecureRestored(filepath.Join(target, "tokens"), selected)
	if errors.Is(err, providercredentials.ErrRestorePermissionsConflict) {
		return 0, fmt.Errorf("%w; selected token files were secured, but token directory permissions were not changed; resolve the conflict or move the omitted entry aside", err)
	}
	if err != nil {
		return 0, fmt.Errorf("%w; credentials may be unreadable and permissions may have changed", err)
	}
	return len(selected), nil
}
