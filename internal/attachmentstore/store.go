// Package attachmentstore adapts Kit's reusable packed-CAS reader to
// msgvault's established attachment-opening interfaces.
package attachmentstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/export"
)

// OpenLooseFile opens a regular attachment file without requiring archive membership.
func OpenLooseFile(path string) (io.ReadCloser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, 0, fmt.Errorf("attachment storage path %q is not a regular file", path)
	}
	return f, info.Size(), nil
}

// OpenLooseCandidates returns the selected path and its recorded index, or -1 for the canonical file.
func OpenLooseCandidates(attachmentsDir, contentHash string, recordedPaths func() ([]string, error), excludedPaths ...string) (io.ReadCloser, int64, int, string, error) {
	path, err := export.StoragePath(attachmentsDir, contentHash)
	if err != nil {
		return nil, 0, 0, "", err
	}
	pathKey := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
		return filepath.Clean(path)
	}
	seen := make(map[string]bool, len(excludedPaths))
	for _, excluded := range excludedPaths {
		seen[pathKey(excluded)] = true
	}
	var reader io.ReadCloser
	var size int64
	if !seen[pathKey(path)] {
		reader, size, err = OpenLooseFile(path)
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return reader, size, -1, path, err
		}
	}
	seen[pathKey(path)] = true
	paths, err := recordedPaths()
	if err != nil {
		return nil, 0, 0, "", err
	}
	var firstPathError error
	for i, storagePath := range paths {
		if storagePath == "" {
			continue
		}
		path, err := ResolveRecordedPath(attachmentsDir, storagePath)
		if err == nil {
			if seen[pathKey(path)] {
				continue
			}
			seen[pathKey(path)] = true
			reader, size, err = OpenLooseFile(path)
			if err == nil {
				return reader, size, i, path, nil
			}
		}
		if !errors.Is(err, os.ErrNotExist) && firstPathError == nil {
			firstPathError = err
		}
	}
	if firstPathError != nil {
		return nil, 0, 0, "", firstPathError
	}
	return nil, 0, 0, "", os.ErrNotExist
}

// ResolveRecordedPath confines a recorded local path to the attachment directory.
func ResolveRecordedPath(attachmentsDir, storagePath string) (string, error) {
	lowerPath := strings.ToLower(storagePath)
	if strings.HasPrefix(lowerPath, "http://") || strings.HasPrefix(lowerPath, "https://") {
		return "", errors.New("attachment storage path must be local")
	}
	localPath := filepath.Clean(filepath.FromSlash(storagePath))
	if !filepath.IsLocal(localPath) {
		return "", fmt.Errorf("attachment storage path %q escapes attachments directory", storagePath)
	}
	basePath, err := filepath.Abs(attachmentsDir)
	if err != nil {
		return "", fmt.Errorf("resolve attachments directory: %w", err)
	}
	basePath, err = filepath.EvalSymlinks(basePath)
	if err != nil {
		return "", fmt.Errorf("resolve attachments directory: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(filepath.Join(basePath, localPath))
	if err != nil {
		return "", err
	}
	relativePath, err := filepath.Rel(basePath, resolvedPath)
	if err != nil || !filepath.IsLocal(relativePath) {
		return "", fmt.Errorf("attachment storage path %q escapes attachments directory", storagePath)
	}
	return resolvedPath, nil
}

// Store is the daemon-owned mixed loose and packed attachment reader.
type Store struct {
	store *packstore.Store
}

// Wrap adapts an existing physical store without changing its ownership.
// The caller remains responsible for closing the underlying store exactly once.
func Wrap(physical *packstore.Store) *Store {
	return &Store{store: physical}
}

// New constructs a store rooted at attachmentsDir using resolver for
// msgvault's attachment-membership and pack-location authority.
func New(resolver packstore.Resolver, attachmentsDir string) (*Store, error) {
	layout, err := packstore.NewLayout(attachmentsDir, packstore.LayoutOptions{
		Staging: packstore.StagingSameDirectory,
	})
	if err != nil {
		return nil, fmt.Errorf("create attachment pack layout: %w", err)
	}
	physical, err := packstore.NewStore(resolver, layout, packstore.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("create attachment pack store: %w", err)
	}
	return Wrap(physical), nil
}

// Open preserves msgvault's established string-hash reader interface.
func (s *Store) Open(hash string) (io.ReadSeekCloser, int64, error) {
	parsed, err := parseHash(hash)
	if err != nil {
		return nil, 0, fmt.Errorf("parse attachment hash: %w", err)
	}
	reader, size, err := s.store.Open(context.Background(), parsed)
	if err != nil {
		return nil, 0, fmt.Errorf("open attachment %s: %w", hash, err)
	}
	return reader, size, nil
}

// OpenStream returns a verified sequential reader without buffering the whole
// attachment. Callers must consume it through EOF (or use the Kit-specific
// verification methods) and must observe the Close error.
func (s *Store) OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	parsed, err := parseHash(hash)
	if err != nil {
		return nil, 0, fmt.Errorf("parse attachment stream hash: %w", err)
	}
	reader, size, err := s.store.OpenStream(ctx, parsed)
	if err != nil {
		return nil, 0, fmt.Errorf("open attachment stream %s: %w", hash, err)
	}
	return reader, size, nil
}

// ReadBounded preserves msgvault's maintenance reader interface.
func (s *Store) ReadBounded(hash string, maxBytes int64) ([]byte, int64, error) {
	parsed, err := parseHash(hash)
	if err != nil {
		return nil, 0, fmt.Errorf("parse bounded attachment hash: %w", err)
	}
	data, size, err := s.store.ReadBounded(context.Background(), parsed, maxBytes)
	if err != nil {
		return nil, 0, fmt.Errorf("read bounded attachment %s: %w", hash, err)
	}
	return data, size, nil
}

func parseHash(hash string) (packstore.Hash, error) {
	if err := export.ValidateContentHash(hash); err != nil {
		return "", err
	}
	parsed, err := packstore.ParseHash(strings.ToLower(hash))
	if err != nil {
		return "", fmt.Errorf("parse canonical attachment hash: %w", err)
	}
	return parsed, nil
}

// RetirePack closes cached readers before physically deleting packID.
func (s *Store) RetirePack(packID string) error {
	if err := s.store.RetirePack(packID); err != nil {
		return fmt.Errorf("retire attachment pack %s: %w", packID, err)
	}
	return nil
}

// Opener adapts verified streaming reads for export consumers.
func (s *Store) Opener() export.AttachmentOpener {
	return func(hash string) (io.ReadCloser, error) {
		reader, _, err := s.OpenStream(context.Background(), hash)
		return reader, err
	}
}

// Close releases cached pack descriptors.
func (s *Store) Close() error {
	if err := s.store.Close(); err != nil {
		return fmt.Errorf("close attachment store: %w", err)
	}
	return nil
}
