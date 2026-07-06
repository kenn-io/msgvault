package backup

import (
	"fmt"

	"go.kenn.io/kit/pack"
)

// PackAppender routes blobs into sealed packs, deduplicating against the
// repository's known blob set plus everything added during this run.
//
// After any returned error, the appender is poisoned and must not be used
// again; callers must call Abort() and discard it. All subsequent Add and
// Finish calls will return the original error without writing.
type PackAppender struct {
	repo       *Repo
	known      map[pack.BlobID]IndexEntry
	zstdLevel  int
	crypter    *pack.Crypter
	targetSize int64

	w          *pack.Writer
	newPacks   []string
	newEntries []IndexEntry
	pending    []IndexEntry // entries for the currently open pack
	err        error        // sticky error: once set, appender is poisoned
}

// NewPackAppender creates an appender. known is mutated: added blobs join it
// so later Adds in the same run dedup against them.
func NewPackAppender(r *Repo, known map[pack.BlobID]IndexEntry, zstdLevel int, crypter *pack.Crypter) *PackAppender {
	return &PackAppender{
		repo:       r,
		known:      known,
		zstdLevel:  zstdLevel,
		crypter:    crypter,
		targetSize: pack.DefaultTargetSize,
	}
}

// Add stores raw as a blob unless it is already known. It returns the blob
// ID and whether a new pack entry was written.
func (a *PackAppender) Add(raw []byte) (pack.BlobID, bool, error) {
	if a.err != nil {
		return pack.BlobID{}, false, a.err
	}
	id := pack.ComputeBlobID(raw)
	if _, ok := a.known[id]; ok {
		return id, false, nil
	}
	if err := a.ensureWriter(); err != nil {
		return pack.BlobID{}, false, err
	}
	entry, err := a.w.Append(raw)
	if err != nil {
		a.err = fmt.Errorf("backup: appending blob %s: %w", id, err)
		return pack.BlobID{}, false, a.err
	}
	return id, true, a.recordEntry(id, entry)
}

// AddEncoded stores one blob whose frame the caller already encoded with
// pack.EncodeFrame, unless it is already known. The caller inherits
// pack.Writer.AppendEncoded's contract: id and (frame, compressed) must
// derive from the same raw bytes. It reports whether a new pack entry was
// written.
func (a *PackAppender) AddEncoded(id pack.BlobID, frame []byte, rawLen uint64, compressed bool) (bool, error) {
	if a.err != nil {
		return false, a.err
	}
	if _, ok := a.known[id]; ok {
		return false, nil
	}
	if err := a.ensureWriter(); err != nil {
		return false, err
	}
	entry, err := a.w.AppendEncoded(id, frame, rawLen, compressed)
	if err != nil {
		a.err = fmt.Errorf("backup: appending blob %s: %w", id, err)
		return false, a.err
	}
	return true, a.recordEntry(id, entry)
}

// knownSnapshot copies the current known-blob set. Concurrent capture
// workers consult the copy to skip compressing already-stored blobs while
// the appender keeps mutating the live map.
func (a *PackAppender) knownSnapshot() map[pack.BlobID]struct{} {
	snap := make(map[pack.BlobID]struct{}, len(a.known))
	for id := range a.known {
		snap[id] = struct{}{}
	}
	return snap
}

func (a *PackAppender) ensureWriter() error {
	if a.w != nil {
		return nil
	}
	w, err := pack.NewWriter(a.repo.Path(stagingDirName), pack.WriterOptions{
		TargetSize: a.targetSize,
		ZstdLevel:  a.zstdLevel,
		Crypter:    a.crypter,
	})
	if err != nil {
		a.err = fmt.Errorf("backup: opening pack writer: %w", err)
		return a.err
	}
	a.w = w
	return nil
}

// recordEntry indexes one appended entry and seals the pack once full.
func (a *PackAppender) recordEntry(id pack.BlobID, entry pack.Entry) error {
	ie := IndexEntry{
		Blob:      entry.ID,
		PackID:    a.w.ID(),
		Offset:    entry.Offset,
		StoredLen: entry.StoredLen,
		Flags:     entry.Flags,
	}
	a.pending = append(a.pending, ie)
	a.known[id] = ie
	if a.w.Full() {
		return a.sealOpen()
	}
	return nil
}

func (a *PackAppender) sealOpen() error {
	if a.w == nil {
		return nil
	}
	id := a.w.ID()
	final := a.repo.Path(packsDirName, id[:2], id+".mvpack")
	if _, err := a.w.Seal(final); err != nil {
		// Poison the appender: abort the writer and mark it as dead so
		// subsequent Add/Finish calls fail immediately. Abort is safe even
		// after a rename (it's a no-op if w.done is true).
		_ = a.w.Abort()
		a.w = nil
		a.pending = nil
		a.err = fmt.Errorf("backup: sealing pack %s: %w", id, err)
		return a.err
	}
	a.newPacks = append(a.newPacks, id)
	a.newEntries = append(a.newEntries, a.pending...)
	a.pending = nil
	a.w = nil
	return nil
}

// Finish seals the open pack (aborting it if empty) and returns the packs
// sealed this run and their index entries.
func (a *PackAppender) Finish() ([]string, []IndexEntry, error) {
	if a.err != nil {
		return nil, nil, a.err
	}
	if a.w != nil && len(a.pending) == 0 {
		if err := a.w.Abort(); err != nil {
			return nil, nil, fmt.Errorf("backup: aborting empty pack: %w", err)
		}
		a.w = nil
	}
	if err := a.sealOpen(); err != nil {
		return nil, nil, err
	}
	return a.newPacks, a.newEntries, nil
}

// Abort discards the open pack; already-sealed packs remain (they are
// unreferenced without a manifest and get ignored, see docs/architecture/backup-format.md, Crash Consistency).
func (a *PackAppender) Abort() {
	if a.w != nil {
		_ = a.w.Abort()
		a.w = nil
	}
}

// ReadBlob fetches one blob by resolving its pack through the index map. The
// pack footer is authoritative for the entry's RawLen and CRC.
func (r *Repo) ReadBlob(known map[pack.BlobID]IndexEntry, id pack.BlobID, crypter *pack.Crypter) ([]byte, error) {
	ie, ok := known[id]
	if !ok {
		return nil, fmt.Errorf("backup: blob %s not present in any index", id)
	}
	pr, err := pack.OpenReader(r.Path(packsDirName, ie.PackID[:2], ie.PackID+".mvpack"), crypter)
	if err != nil {
		return nil, fmt.Errorf("backup: opening pack %s for blob %s: %w", ie.PackID, id, err)
	}
	defer func() { _ = pr.Close() }()
	for _, e := range pr.Entries() {
		if e.ID == id {
			raw, err := pr.ReadBlob(e)
			if err != nil {
				return nil, fmt.Errorf("backup: reading blob %s from pack %s: %w", id, ie.PackID, err)
			}
			return raw, nil
		}
	}
	return nil, fmt.Errorf("backup: blob %s not found in pack %s (index inconsistency)", id, ie.PackID)
}
