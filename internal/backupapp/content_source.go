package backupapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"go.kenn.io/kit/backup"
)

// BlobStore is the production mixed-storage read contract used by capture.
type BlobStore interface {
	OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error)
}

// NewContentSource returns a backup.ContentSource that serves attachment
// bytes from the production blob store (packs plus canonical loose files),
// falling back to the DB-recorded noncanonical relative path under
// attachmentsDir for legacy blobs the packer has not canonicalized yet.
func NewContentSource(blobs BlobStore, attachmentsDir string) backup.ContentSource {
	return &blobSource{blobs: blobs, attachmentsDir: attachmentsDir}
}

// blobSource serves capture reads from the production blob store (packs +
// canonical loose files), falling back to the DB-recorded noncanonical
// relative path for legacy blobs the packer has not canonicalized yet
// (e.g. synctech-sms/<aa>/<hash>). Open is concurrency-safe (blobstore
// serializes packed reads; loose reads are independent os.Open calls).
type blobSource struct {
	blobs          BlobStore
	attachmentsDir string
}

// Open implements backup.ContentSource. ref.Hash is validated by
// blobs.OpenStream
// before any slicing: the [:2] slices below run only after Open returned
// fs.ErrNotExist, which a malformed hash never produces (its validation
// error does not satisfy fs.ErrNotExist).
func (s *blobSource) Open(ctx context.Context, ref backup.ContentRef) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, _, err := s.blobs.OpenStream(ctx, ref.Hash)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	rel := ref.StoragePath
	if rel == "" || rel == ref.Hash[:2]+"/"+ref.Hash {
		return nil, err // nothing more to try
	}
	native := filepath.FromSlash(rel)
	if !filepath.IsLocal(native) {
		return nil, fmt.Errorf("attachment %s has non-local storage path %q", ref.Hash, rel)
	}
	f, ferr := os.Open(filepath.Join(s.attachmentsDir, native))
	if ferr != nil {
		if errors.Is(ferr, fs.ErrNotExist) {
			// The packer may have committed the blob's pack mapping and
			// removed this legacy file after the initial blob-store lookup.
			// Retry once so the now-authoritative packed location wins.
			r, _, retryErr := s.blobs.OpenStream(ctx, ref.Hash)
			if retryErr == nil {
				return r, nil
			}
			if !errors.Is(retryErr, fs.ErrNotExist) {
				return nil, retryErr
			}
		}
		return nil, fmt.Errorf("attachment %s not in packs, canonical loose, or recorded path %q: %w",
			ref.Hash, rel, ferr)
	}
	return f, nil
}
