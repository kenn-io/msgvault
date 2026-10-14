package emlx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Fingerprint recognizes stable filesystem metadata without reading content.
// It is a cache hint, not a content-integrity snapshot. Unsupported identities,
// links, special files and excessively large dependency trees remain cold.
func Fingerprint(ctx context.Context, path string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false, err
	}
	if ok, err := plainAncestors(ctx, abs, false); err != nil || !ok {
		return "", false, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return "", false, err
	}
	if !fi.Mode().IsRegular() {
		return "", false, nil
	}
	h := sha256.New()
	add := func(name string, info fs.FileInfo) bool {
		identity, ok := fileIdentity(info)
		if !ok {
			return false
		}
		_, _ = fmt.Fprintf(h, "%d:%s:%d:%d:%d:%s\n", len(name), name, info.Size(), info.ModTime().UnixNano(), info.Mode(), identity)
		return true
	}
	if !add(abs, fi) {
		return "", false, nil
	}
	dep := attachmentsDir(abs)
	if dep != "" {
		if ok, err := plainAncestors(ctx, dep, true); err != nil || !ok {
			return "", false, err
		}
		visited := 0
		eligible := true
		err = filepath.WalkDir(dep, func(p string, d fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				if p == dep && errors.Is(walkErr, os.ErrNotExist) {
					_, _ = fmt.Fprint(h, "attachments:absent\n")
					return nil
				}
				return walkErr
			}
			visited++
			if visited > 10000 {
				eligible = false
				return fs.SkipAll
			}
			if d.Type()&os.ModeSymlink != 0 {
				eligible = false
				return fs.SkipAll
			}
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("stat attachment dependency: %w", err)
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				eligible = false
				return fs.SkipAll
			}
			rel, err := filepath.Rel(dep, p)
			if err != nil {
				return err
			}
			if !add(filepath.ToSlash(rel), info) {
				eligible = false
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			return "", false, fmt.Errorf("attachment fingerprint: %w", err)
		}
		if !eligible {
			return "", false, nil
		}
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}

func plainAncestors(ctx context.Context, p string, optional bool) (bool, error) {
	volume := filepath.VolumeName(p)
	current := volume + string(filepath.Separator)
	for component := range strings.SplitSeq(strings.TrimPrefix(p, current), string(filepath.Separator)) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			if optional && errors.Is(err, os.ErrNotExist) {
				return true, nil
			}
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, nil
		}
	}
	return true, nil
}
