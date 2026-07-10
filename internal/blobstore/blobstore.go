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

// maxOpenReaders bounds cached pack slots. A slot may own both kit's ordinary
// reader and blobstore's bounded reader for the same pack.
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

	// mu guards both reader caches/order and is held across packed reads so
	// an evicted descriptor is never closed while another goroutine uses it.
	// Packed reads are short (one pread + optional zstd decode).
	mu             sync.Mutex
	readers        map[string]*ordinaryPackReader
	boundedReaders map[string]*boundedPackReader
	order          []string
}

// New creates a blob store over attachmentsDir backed by index.
func New(index PackIndex, attachmentsDir string) *Store {
	return &Store{
		index:          index,
		attachmentsDir: attachmentsDir,
		readers:        make(map[string]*ordinaryPackReader),
		boundedReaders: make(map[string]*boundedPackReader),
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
	return resolveBlob(s, hash, s.openLoose, s.openPacked)
}

// ReadBounded returns verified blob bytes directly while enforcing maxBytes
// against both raw and stored representations. Packed cache misses also apply
// the fixed maintenance container/footer/entry ceilings.
func (s *Store) ReadBounded(hash string, maxBytes int64) ([]byte, int64, error) {
	if err := export.ValidateContentHash(hash); err != nil {
		return nil, 0, err
	}
	if maxBytes < 0 {
		return nil, 0, fmt.Errorf("bounded attachment limit must be nonnegative, got %d", maxBytes)
	}
	return resolveBlob(s, hash,
		func(hash string) ([]byte, int64, error) {
			return s.readLooseBounded(hash, maxBytes)
		},
		func(hash string, entry *store.PackIndexEntry) ([]byte, int64, error) {
			return s.readPackedBounded(hash, entry, maxBytes)
		})
}

// resolveBlob centralizes the index resolution and single-retry rules shared
// by streaming and bounded reads.
func resolveBlob[T any](s *Store, hash string,
	readLoose func(string) (T, int64, error),
	readPacked func(string, *store.PackIndexEntry) (T, int64, error),
) (T, int64, error) {
	var zero T
	loc, err := s.index.ResolveAttachmentBlob(hash)
	if err != nil {
		return zero, 0, err
	}
	if !loc.Referenced {
		return zero, 0, blobNotFound(hash)
	}
	if loc.Pack == nil {
		r, size, looseErr := readLoose(hash)
		if !errors.Is(looseErr, fs.ErrNotExist) {
			return r, size, looseErr
		}
		loc, err = s.index.ResolveAttachmentBlob(hash)
		if err != nil {
			return zero, 0, err
		}
		if !loc.Referenced {
			return zero, 0, blobNotFound(hash)
		}
		if loc.Pack == nil {
			return zero, 0, looseErr
		}
		return readPacked(hash, loc.Pack)
	}
	r, size, packErr := readPacked(hash, loc.Pack)
	if !errors.Is(packErr, fs.ErrNotExist) {
		return r, size, packErr
	}
	loc, err = s.index.ResolveAttachmentBlob(hash)
	if err != nil {
		return zero, 0, err
	}
	if !loc.Referenced {
		return zero, 0, blobNotFound(hash)
	}
	if loc.Pack == nil {
		return readLoose(hash)
	}
	return readPacked(hash, loc.Pack)
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
	ids := make(map[string]struct{}, len(s.readers)+len(s.boundedReaders))
	for id := range s.readers {
		ids[id] = struct{}{}
	}
	for id := range s.boundedReaders {
		ids[id] = struct{}{}
	}
	var closeErr error
	for id := range ids {
		closeErr = errors.Join(closeErr, s.closePackSlotLocked(id))
	}
	s.readers = make(map[string]*ordinaryPackReader)
	s.boundedReaders = make(map[string]*boundedPackReader)
	s.order = nil
	return closeErr
}

// RetirePack closes and forgets the daemon-owned cached reader for packID,
// then removes the canonical pack file while packed reads remain excluded.
// It deliberately does not alter database metadata; callers may remove a
// zero-live pack record only after this physical retirement succeeds.
func (s *Store) RetirePack(packID string) error {
	if !pack.IsValidPackID(packID) {
		return fmt.Errorf("invalid pack id %q", packID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	order := s.order[:0]
	for _, id := range s.order {
		if id != packID {
			order = append(order, id)
		}
	}
	s.order = order

	closeErr := s.closePackSlotLocked(packID)
	path := filepath.Join(s.attachmentsDir, "packs", packID[:2], packID+PackExt)
	var removeErr error
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		removeErr = fmt.Errorf("remove pack %s: %w", packID, err)
	}
	return errors.Join(closeErr, removeErr)
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

func (s *Store) readLooseBounded(hash string, maxBytes int64) ([]byte, int64, error) {
	p, err := export.StoragePath(s.attachmentsDir, hash)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("stat loose attachment %s: %w", hash, err)
	}
	size := st.Size()
	if size < 0 {
		return nil, 0, fmt.Errorf("stat loose attachment %s: negative size %d", hash, size)
	}
	if size > maxBytes {
		return nil, 0, newLimitError(LimitBlobRawBytes, uint64(size), uint64(maxBytes)) //nolint:gosec // nonnegative size and validated limit
	}
	maxInt := uint64(^uint(0) >> 1)
	if uint64(size) > maxInt {
		return nil, 0, newLimitError(LimitBlobRawBytes, uint64(size), maxInt)
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, 0, fmt.Errorf("read loose attachment %s: %w", hash, err)
	}
	var probe [1]byte
	n, err := f.Read(probe[:])
	if n != 0 {
		return nil, 0, newLimitError(LimitBlobStatBytes, uint64(size)+uint64(n), uint64(size)) //nolint:gosec // nonnegative descriptor stat
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, fmt.Errorf("probe loose attachment %s for growth: %w", hash, err)
	}
	return data, size, nil
}

func (s *Store) openPacked(hash string, e *store.PackIndexEntry) (io.ReadSeekCloser, int64, error) {
	if !pack.IsValidPackID(e.PackID) {
		return nil, 0, fmt.Errorf("invalid pack id %q in index for blob %s", e.PackID, hash)
	}
	blobID, err := pack.ParseBlobID(hash)
	if err != nil {
		return nil, 0, fmt.Errorf("parse blob id %s: %w", hash, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.readerLocked(e.PackID)
	if err != nil {
		return nil, 0, err
	}
	entryIndex, found := r.entryIndexes[blobID]
	if !found {
		return nil, 0, &fs.PathError{
			Op:   "find attachment blob in pack footer",
			Path: hash,
			Err:  fs.ErrNotExist,
		}
	}
	footerEntry := r.Entries()[entryIndex]
	if r.ID() != e.PackID || !packIndexMatchesFooter(e, footerEntry) {
		return nil, 0, fmt.Errorf("pack index metadata mismatch for blob %s in pack %s", hash, e.PackID)
	}
	data, err := r.ReadBlob(footerEntry)
	if err != nil {
		return nil, 0, fmt.Errorf("read blob %s from pack %s: %w", hash, e.PackID, err)
	}
	return nopSeekCloser{bytes.NewReader(data)}, int64(len(data)), nil
}

func (s *Store) readPackedBounded(hash string, e *store.PackIndexEntry, maxBytes int64) ([]byte, int64, error) {
	if !pack.IsValidPackID(e.PackID) {
		return nil, 0, fmt.Errorf("invalid pack id %q in index for blob %s", e.PackID, hash)
	}
	blobID, err := pack.ParseBlobID(hash)
	if err != nil {
		return nil, 0, fmt.Errorf("parse blob id %s: %w", hash, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.boundedReaderLocked(e.PackID)
	if err != nil {
		return nil, 0, err
	}
	if len(r.entries) > MaxMaintenancePackEntries {
		return nil, 0, newLimitError(LimitPackEntryCount, uint64(len(r.entries)), MaxMaintenancePackEntries)
	}
	footerEntry, found := r.entries[blobID]
	if !found {
		return nil, 0, &fs.PathError{
			Op:   "find attachment blob in pack footer",
			Path: hash,
			Err:  fs.ErrNotExist,
		}
	}
	if !packIndexMatchesFooter(e, footerEntry) {
		return nil, 0, fmt.Errorf("pack index metadata mismatch for blob %s in pack %s", hash, e.PackID)
	}
	data, err := r.readBlob(footerEntry, maxBytes)
	if err != nil {
		return nil, 0, fmt.Errorf("read bounded blob %s from pack %s: %w", hash, e.PackID, err)
	}
	return data, int64(len(data)), nil
}

// readerLocked returns a cached reader for the pack, opening and caching it
// (with FIFO eviction) on miss. Caller holds s.mu.
func (s *Store) readerLocked(packID string) (*ordinaryPackReader, error) {
	if r, ok := s.readers[packID]; ok {
		return r, nil
	}
	p := filepath.Join(s.attachmentsDir, "packs", packID[:2], packID+PackExt)
	kitReader, err := pack.OpenReader(p, nil)
	if err != nil {
		// %w preserves errors.Is(err, fs.ErrNotExist) through the wrap, which
		// Open's retry rule depends on.
		return nil, fmt.Errorf("open pack %s: %w", packID, err)
	}
	footerEntries := kitReader.Entries()
	entryIndexes := make(map[pack.BlobID]int, len(footerEntries))
	for i, entry := range footerEntries {
		if _, duplicate := entryIndexes[entry.ID]; duplicate {
			_ = kitReader.Close()
			return nil, fmt.Errorf("%w: duplicate blob id %s in pack %s footer",
				pack.ErrCorrupt, entry.ID, packID)
		}
		entryIndexes[entry.ID] = i
	}
	r := &ordinaryPackReader{Reader: kitReader, entryIndexes: entryIndexes}
	s.addPackSlotLocked(packID)
	s.readers[packID] = r
	return r, nil
}

func packIndexMatchesFooter(index *store.PackIndexEntry, footer pack.Entry) bool {
	return index.BlobHash == footer.ID.String() &&
		index.Offset >= 0 && uint64(index.Offset) == footer.Offset &&
		index.StoredLen >= 0 && uint64(index.StoredLen) == footer.StoredLen &&
		index.RawLen >= 0 && uint64(index.RawLen) == footer.RawLen &&
		pack.BlobFlags(index.Flags) == footer.Flags && index.CRC32C == footer.CRC32C
}

// boundedReaderLocked opens, validates, and caches the exact descriptor used
// by bounded stored reads. Caller holds s.mu.
func (s *Store) boundedReaderLocked(packID string) (*boundedPackReader, error) {
	if r, ok := s.boundedReaders[packID]; ok {
		if len(r.entries) > MaxMaintenancePackEntries {
			return nil, newLimitError(LimitPackEntryCount, uint64(len(r.entries)), MaxMaintenancePackEntries)
		}
		return r, nil
	}
	p := filepath.Join(s.attachmentsDir, "packs", packID[:2], packID+PackExt)
	r, err := openBoundedPack(p)
	if err != nil {
		return nil, fmt.Errorf("open bounded pack %s: %w", packID, err)
	}
	if len(r.entries) > MaxMaintenancePackEntries {
		_ = r.Close()
		return nil, newLimitError(LimitPackEntryCount, uint64(len(r.entries)), MaxMaintenancePackEntries)
	}
	s.addPackSlotLocked(packID)
	s.boundedReaders[packID] = r
	return r, nil
}

// addPackSlotLocked adds packID to the shared FIFO if neither cache already
// owns that pack. Ordinary and bounded readers for one pack share one slot.
func (s *Store) addPackSlotLocked(packID string) {
	if _, ok := s.readers[packID]; ok {
		return
	}
	if _, ok := s.boundedReaders[packID]; ok {
		return
	}
	if len(s.order) >= maxOpenReaders {
		oldest := s.order[0]
		s.order = s.order[1:]
		_ = s.closePackSlotLocked(oldest)
	}
	s.order = append(s.order, packID)
}

// closePackSlotLocked closes and removes both reader forms for packID.
func (s *Store) closePackSlotLocked(packID string) error {
	var closeErr error
	if r, ok := s.readers[packID]; ok {
		if err := r.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close pack reader %s: %w", packID, err))
		}
		delete(s.readers, packID)
	}
	if r, ok := s.boundedReaders[packID]; ok {
		if err := r.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close bounded pack reader %s: %w", packID, err))
		}
		delete(s.boundedReaders, packID)
	}
	return closeErr
}

// ordinaryPackReader keeps kit's verified reader and an immutable O(1)
// footer lookup built once when the cache slot opens.
type ordinaryPackReader struct {
	*pack.Reader

	entryIndexes map[pack.BlobID]int
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }
