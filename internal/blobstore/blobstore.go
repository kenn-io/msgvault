// Package blobstore reads attachment content by SHA-256 hash from packed CAS
// storage (sealed kit pack files under <attachmentsDir>/packs/) with a
// fallback to loose <hash[:2]>/<hash> files. It is the single read path for
// attachment bytes; see docs/internal/packed-attachments-design.md.
package blobstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

// PackExt matches the backup engine's pack file extension so a future
// release can share packs between production and backup repos.
const PackExt = ".mvpack"

// maxOpenReaders bounds the cache of open pack readers (file handle plus
// parsed footer each).
const maxOpenReaders = 16

// PackIndex resolves both attachment liveness and its optional pack location
// in one lookup. *store.Store implements it via ResolveAttachmentBlob.
type PackIndex interface {
	ResolveAttachmentBlob(blobHash string) (store.AttachmentBlobLocation, error)
}

// Store reads attachment blobs from packs with a loose-file fallback.
type Store struct {
	index          PackIndex
	attachmentsDir string

	// mu guards readers/order and is held across packed reads so an evicted
	// reader is never closed while another goroutine is mid-ReadBlob.
	// Packed reads are short (one pread + optional zstd decode).
	mu      sync.Mutex
	readers map[string]*pack.Reader
	order   []string
}

// New creates a blob store over attachmentsDir backed by index.
func New(index PackIndex, attachmentsDir string) *Store {
	return &Store{
		index:          index,
		attachmentsDir: attachmentsDir,
		readers:        make(map[string]*pack.Reader),
	}
}

// Open returns the blob with the given SHA-256 content hash and its size,
// preferring packed storage. Not-found satisfies errors.Is(err, fs.ErrNotExist).
//
// Two benign races with the (future) packer and repacker are absorbed by
// retrying the index lookup once: a loose file deleted just after an index
// miss, and a pack deleted just after a stale index hit.
func (s *Store) Open(hash string) (io.ReadSeekCloser, int64, error) {
	if err := export.ValidateContentHash(hash); err != nil {
		return nil, 0, err
	}
	loc, err := s.index.ResolveAttachmentBlob(hash)
	if err != nil {
		return nil, 0, err
	}
	if !loc.Referenced {
		return nil, 0, blobNotFound(hash)
	}
	if loc.Pack == nil {
		r, size, looseErr := s.openLoose(hash)
		if !errors.Is(looseErr, fs.ErrNotExist) {
			return r, size, looseErr
		}
		loc, err = s.index.ResolveAttachmentBlob(hash)
		if err != nil {
			return nil, 0, err
		}
		if !loc.Referenced {
			return nil, 0, blobNotFound(hash)
		}
		if loc.Pack == nil {
			return nil, 0, looseErr
		}
		return s.openPacked(hash, loc.Pack)
	}
	r, size, packErr := s.openPacked(hash, loc.Pack)
	if !errors.Is(packErr, fs.ErrNotExist) {
		return r, size, packErr
	}
	loc, err = s.index.ResolveAttachmentBlob(hash)
	if err != nil {
		return nil, 0, err
	}
	if !loc.Referenced {
		return nil, 0, blobNotFound(hash)
	}
	if loc.Pack == nil {
		return s.openLoose(hash)
	}
	return s.openPacked(hash, loc.Pack)
}

func blobNotFound(hash string) error {
	return &fs.PathError{Op: "open attachment blob", Path: hash, Err: fs.ErrNotExist}
}

// Opener adapts Open to the export package's opener callback.
func (s *Store) Opener() export.AttachmentOpener {
	return func(contentHash string) (io.ReadCloser, error) {
		r, _, err := s.Open(contentHash)
		return r, err
	}
}

// Close releases all cached pack readers.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for id, r := range s.readers {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close pack reader %s: %w", id, err)
		}
	}
	s.readers = make(map[string]*pack.Reader)
	s.order = nil
	return firstErr
}

func (s *Store) openLoose(hash string) (io.ReadSeekCloser, int64, error) {
	p, err := export.StoragePath(s.attachmentsDir, hash)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat loose attachment %s: %w", hash, err)
	}
	return f, st.Size(), nil
}

func (s *Store) openPacked(hash string, e *store.PackIndexEntry) (io.ReadSeekCloser, int64, error) {
	if !pack.IsValidPackID(e.PackID) {
		return nil, 0, fmt.Errorf("invalid pack id %q in index for blob %s", e.PackID, hash)
	}
	blobID, err := pack.ParseBlobID(hash)
	if err != nil {
		return nil, 0, fmt.Errorf("parse blob id %s: %w", hash, err)
	}
	pe := pack.Entry{
		ID:        blobID,
		Offset:    uint64(e.Offset),    //nolint:gosec // column mirrors a uint64
		StoredLen: uint64(e.StoredLen), //nolint:gosec // column mirrors a uint64
		RawLen:    uint64(e.RawLen),    //nolint:gosec // column mirrors a uint64
		Flags:     pack.BlobFlags(e.Flags),
		CRC32C:    e.CRC32C,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.readerLocked(e.PackID)
	if err != nil {
		return nil, 0, err
	}
	data, err := r.ReadBlob(pe)
	if err != nil {
		return nil, 0, fmt.Errorf("read blob %s from pack %s: %w", hash, e.PackID, err)
	}
	return nopSeekCloser{bytes.NewReader(data)}, int64(len(data)), nil
}

// readerLocked returns a cached reader for the pack, opening and caching it
// (with FIFO eviction) on miss. Caller holds s.mu.
func (s *Store) readerLocked(packID string) (*pack.Reader, error) {
	if r, ok := s.readers[packID]; ok {
		return r, nil
	}
	p := filepath.Join(s.attachmentsDir, "packs", packID[:2], packID+PackExt)
	r, err := pack.OpenReader(p, nil)
	if err != nil {
		// %w preserves errors.Is(err, fs.ErrNotExist) through the wrap, which
		// Open's retry rule depends on.
		return nil, fmt.Errorf("open pack %s: %w", packID, err)
	}
	if len(s.order) >= maxOpenReaders {
		oldest := s.order[0]
		s.order = s.order[1:]
		if old, ok := s.readers[oldest]; ok {
			_ = old.Close()
			delete(s.readers, oldest)
		}
	}
	s.readers[packID] = r
	s.order = append(s.order, packID)
	return r, nil
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }
