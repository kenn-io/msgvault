package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"go.kenn.io/kit/pack"
)

// ExtrasOptions selects which operational files ride along with a snapshot
// (docs/usage/backup.md, Extras).
type ExtrasOptions struct {
	DataDir               string
	ConfigPath            string
	IncludeConfig         bool
	IncludeTokens         bool
	AllowPlaintextSecrets bool
	Encrypted             bool
}

// ExtrasEntry is one captured file in the extras tree.
type ExtrasEntry struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Size int64  `json:"size"`
	Blob string `json:"blob"`
}

// ExtrasTree is the small JSON tree object referenced by the manifest.
type ExtrasTree struct {
	Entries []ExtrasEntry `json:"entries"`
}

// CaptureExtras stores extras file blobs and the tree object.
func CaptureExtras(opts ExtrasOptions, appender *PackAppender) (pack.BlobID, bool, error) {
	// config.toml carries API keys (server.api_key) verbatim and the tokens
	// directory holds OAuth secrets, so either flag on an unencrypted repo
	// needs the explicit plaintext override. Fail safe by naming the flag(s)
	// that triggered the guard.
	if (opts.IncludeConfig || opts.IncludeTokens) && !opts.Encrypted && !opts.AllowPlaintextSecrets {
		var flag string
		switch {
		case opts.IncludeConfig && opts.IncludeTokens:
			flag = "--include-config/--include-tokens"
		case opts.IncludeConfig:
			flag = "--include-config"
		default:
			flag = "--include-tokens"
		}
		return pack.BlobID{}, false, fmt.Errorf(
			"backup: %s requires an encrypted repository (use --allow-plaintext-secrets to override)", flag)
	}
	var entries []ExtrasEntry
	addFile := func(absPath, relPath string) error {
		content, err := os.ReadFile(absPath)
		if err != nil {
			return fmt.Errorf("backup: reading extras file %s: %w", relPath, err)
		}
		info, err := os.Stat(absPath)
		if err != nil {
			return fmt.Errorf("backup: stat extras file %s: %w", relPath, err)
		}
		id, _, err := appender.Add(content)
		if err != nil {
			return err
		}
		entries = append(entries, ExtrasEntry{
			Path: filepath.ToSlash(relPath),
			Mode: uint32(info.Mode().Perm()),
			Size: int64(len(content)),
			Blob: id.String(),
		})
		return nil
	}
	addDir := func(name string) error {
		root := filepath.Join(opts.DataDir, name)
		if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("backup: walking %s: %w", name, err)
			}
			if d.IsDir() {
				return nil
			}
			// Reject non-regular files: symlinks, sockets, devices, etc.
			// Symlinks are an attack surface — they could point outside DataDir.
			if !d.Type().IsRegular() {
				if d.Type()&fs.ModeSymlink != 0 {
					rel, err := filepath.Rel(opts.DataDir, path)
					if err != nil {
						rel = path
					}
					return fmt.Errorf("extras: %s is not a regular file", filepath.ToSlash(rel))
				}
				// Skip other non-regular types silently.
				return nil
			}
			rel, err := filepath.Rel(opts.DataDir, path)
			if err != nil {
				return err
			}
			return addFile(path, rel)
		})
	}
	if err := addDir("deletions"); err != nil {
		return pack.BlobID{}, false, err
	}
	if opts.IncludeConfig && opts.ConfigPath != "" {
		if err := addFile(opts.ConfigPath, "config.toml"); err != nil {
			return pack.BlobID{}, false, err
		}
	}
	if opts.IncludeTokens {
		if err := addDir("tokens"); err != nil {
			return pack.BlobID{}, false, err
		}
		secrets, err := filepath.Glob(filepath.Join(opts.DataDir, "client_secret*.json"))
		if err != nil {
			return pack.BlobID{}, false, fmt.Errorf("backup: globbing client secrets: %w", err)
		}
		for _, s := range secrets {
			rel, err := filepath.Rel(opts.DataDir, s)
			if err != nil {
				return pack.BlobID{}, false, err
			}
			// filepath.Glob doesn't walk a directory tree, so it bypasses
			// addDir's symlink rejection; os.ReadFile inside addFile would
			// otherwise happily follow a symlink outside DataDir. Lstat (not
			// Stat) reports the link itself rather than its target.
			info, err := os.Lstat(s)
			if err != nil {
				return pack.BlobID{}, false, fmt.Errorf("backup: stat extras file %s: %w", rel, err)
			}
			if !info.Mode().IsRegular() {
				return pack.BlobID{}, false, fmt.Errorf("extras: %s is not a regular file", filepath.ToSlash(rel))
			}
			if err := addFile(s, rel); err != nil {
				return pack.BlobID{}, false, err
			}
		}
	}
	if len(entries) == 0 {
		return pack.BlobID{}, false, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	data, err := json.MarshalIndent(&ExtrasTree{Entries: entries}, "", "  ")
	if err != nil {
		return pack.BlobID{}, false, fmt.Errorf("backup: marshaling extras tree: %w", err)
	}
	id, _, err := appender.Add(data)
	if err != nil {
		return pack.BlobID{}, false, err
	}
	return id, true, nil
}
