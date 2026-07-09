// Package packer migrates loose content-addressed attachment blobs into
// sealed kit pack files under <attachmentsDir>/packs/, reconciling crash
// leftovers and sweeping indexed loose files. See
// docs/internal/packed-attachments-design.md.
package packer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/store"
)

// Options tunes one packer run. Zero values select defaults.
type Options struct {
	// TargetSize is the pack size threshold; 0 means pack.DefaultTargetSize.
	TargetSize int64
}

// Stats summarizes one packer run.
type Stats struct {
	PacksSealed  int   // packs written this run
	BlobsPacked  int   // blobs appended this run
	BytesPacked  int64 // raw bytes appended this run
	PacksAdopted int   // orphan packs adopted during reconciliation
	PacksRemoved int   // fully-redundant orphan packs deleted
	BlobsMissing int   // enumerated blobs whose file was missing (left for backfill)
	BlobsCorrupt int   // files whose bytes did not match their recorded hash (skipped)
	LooseSwept   int   // indexed loose files removed by the sweep
}

// Run packs all unindexed loose attachment blobs into sealed packs,
// reconciling crash leftovers first and sweeping indexed loose files after.
// The caller must hold the archive's exclusive-writer coverage (daemon
// operation gate or db.write.lock).
func Run(ctx context.Context, st *store.Store, attachmentsDir string, opts Options) (Stats, error) {
	var stats Stats
	attachmentsDir = filepath.Clean(attachmentsDir)
	packsDir := filepath.Join(attachmentsDir, "packs")
	if err := os.MkdirAll(packsDir, 0o700); err != nil {
		return stats, fmt.Errorf("create packs dir: %w", err)
	}
	if err := cleanStaging(packsDir); err != nil {
		return stats, err
	}
	if err := reconcilePacks(st, packsDir, &stats); err != nil {
		return stats, fmt.Errorf("reconcile orphan packs: %w", err)
	}
	if err := packLoose(ctx, st, attachmentsDir, packsDir, opts, &stats); err != nil {
		return stats, err
	}
	if err := sweepIndexed(st, attachmentsDir, packsDir, &stats); err != nil {
		return stats, err
	}
	return stats, nil
}

// cleanStaging removes every *.staging file directly under packsDir. The
// packer runs exclusively, so any staging file is a dead mid-seal abort.
func cleanStaging(packsDir string) error {
	entries, err := os.ReadDir(packsDir)
	if err != nil {
		return fmt.Errorf("read packs dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".staging") {
			continue
		}
		p := filepath.Join(packsDir, e.Name())
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("remove stale staging file %s: %w", p, err)
		}
		slog.Info("removed stale pack staging file", "path", p)
	}
	return nil
}

// reconcilePacks walks packsDir for *.mvpack files with no attachment_packs
// row and adopts or removes each one.
func reconcilePacks(st *store.Store, packsDir string, stats *Stats) error {
	return filepath.WalkDir(packsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), blobstore.PackExt) {
			return nil
		}
		id := strings.TrimSuffix(d.Name(), blobstore.PackExt)
		if !pack.IsValidPackID(id) {
			slog.Warn("skipping pack file with invalid pack id", "path", path)
			return nil
		}
		has, err := st.HasPackRecord(id)
		if err != nil {
			return err
		}
		if has {
			return nil
		}
		return reconcileOnePack(st, path, id, stats)
	})
}

// reconcileOnePack adopts an orphan pack's unindexed, verified entries, or
// removes the pack when every entry is already indexed elsewhere.
func reconcileOnePack(st *store.Store, path, id string, stats *Stats) error {
	r, err := pack.OpenReader(path, nil)
	if err != nil {
		slog.Warn("skipping unreadable orphan pack", "pack", id, "error", err)
		return nil
	}
	adoptable, rec, err := collectAdoptable(st, r, id)
	if closeErr := r.Close(); closeErr != nil {
		slog.Warn("close orphan pack reader", "pack", id, "error", closeErr)
	}
	if err != nil {
		return err
	}
	if len(adoptable) == 0 {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove redundant orphan pack %s: %w", id, err)
		}
		stats.PacksRemoved++
		slog.Info("removed fully-redundant orphan pack", "pack", id)
		return nil
	}
	if err := st.RecordPackedBlobs(rec, adoptable); err != nil {
		return fmt.Errorf("adopt orphan pack %s: %w", id, err)
	}
	stats.PacksAdopted++
	slog.Info("adopted orphan pack", "pack", id, "entries", len(adoptable))
	return nil
}

// collectAdoptable returns the orphan pack's verified entries that have no
// index row, plus a PackRecord carrying the FULL footer totals (design:
// "footer entry count at seal/adoption").
func collectAdoptable(st *store.Store, r *pack.Reader, id string) ([]store.PackIndexEntry, store.PackRecord, error) {
	entries := r.Entries()
	rec := store.PackRecord{
		PackID:     id,
		EntryCount: int64(len(entries)),
		CreatedAt:  time.Now().UTC(),
	}
	var adoptable []store.PackIndexEntry
	for _, e := range entries {
		rec.StoredBytes += int64(e.StoredLen) //nolint:gosec // stored lengths are bounded by pack.MaxRawLen
		hash := e.ID.String()
		existing, err := st.GetAttachmentPackEntry(hash)
		if err != nil {
			return nil, rec, err
		}
		if existing != nil {
			continue
		}
		if _, err := r.ReadBlob(e); err != nil {
			slog.Warn("orphan pack entry failed verification; not adopting",
				"pack", id, "hash", hash, "error", err)
			continue
		}
		adoptable = append(adoptable, indexEntry(id, e))
	}
	return adoptable, rec, nil
}

// packLoose appends every unindexed loose blob to pack writers, sealing and
// committing each pack as it fills (and the final partial pack).
func packLoose(ctx context.Context, st *store.Store, attachmentsDir, packsDir string, opts Options, stats *Stats) error {
	blobs, err := st.ListUnpackedBlobs()
	if err != nil {
		return err
	}
	var w *pack.Writer
	var sources []string
	abort := func() {
		if w != nil {
			if err := w.Abort(); err != nil {
				slog.Warn("abort pack writer", "error", err)
			}
		}
	}
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			abort()
			return err
		}
		data, ok := readLooseBlob(attachmentsDir, b, stats)
		if !ok {
			continue
		}
		if w == nil {
			w, err = pack.NewWriter(packsDir, pack.WriterOptions{
				TargetSize: opts.TargetSize, ZstdLevel: pack.DefaultZstdLevel,
			})
			if err != nil {
				return fmt.Errorf("create pack writer: %w", err)
			}
			sources = sources[:0]
		}
		if _, err := w.Append(data); err != nil {
			abort()
			return fmt.Errorf("append blob %s to pack %s: %w", b.Hash, w.ID(), err)
		}
		stats.BlobsPacked++
		stats.BytesPacked += int64(len(data))
		sources = append(sources, filepath.Join(attachmentsDir, filepath.FromSlash(b.Path)))
		if w.Full() {
			if err := sealAndCommit(st, packsDir, w, sources, stats); err != nil {
				return err
			}
			w = nil
		}
	}
	if w != nil {
		return sealAndCommit(st, packsDir, w, sources, stats)
	}
	return nil
}

// readLooseBlob loads and verifies one enumerated blob's bytes; a false
// return means the blob was skipped (counted in stats where applicable).
func readLooseBlob(attachmentsDir string, b store.UnpackedBlob, stats *Stats) ([]byte, bool) {
	rel := filepath.FromSlash(b.Path)
	if !filepath.IsLocal(rel) {
		slog.Warn("skipping blob with non-local recorded path", "hash", b.Hash, "path", b.Path)
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(attachmentsDir, rel))
	if errors.Is(err, fs.ErrNotExist) {
		stats.BlobsMissing++
		slog.Warn("loose blob file missing; left for backfill", "hash", b.Hash, "path", b.Path)
		return nil, false
	}
	if err != nil {
		slog.Warn("skipping unreadable loose blob", "hash", b.Hash, "path", b.Path, "error", err)
		return nil, false
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != b.Hash {
		stats.BlobsCorrupt++
		slog.Error("loose blob bytes do not match recorded hash; skipping",
			"hash", b.Hash, "path", b.Path)
		return nil, false
	}
	return data, true
}

// sealAndCommit seals the writer to its final sharded path, records the pack
// and its index rows (canonicalizing recorded paths in the same transaction),
// and removes the packed source files. If recording fails, the sealed pack is
// adopted by reconciliation on the next run (design crash boundary).
func sealAndCommit(st *store.Store, packsDir string, w *pack.Writer, sources []string, stats *Stats) error {
	id := w.ID()
	finalPath := filepath.Join(packsDir, id[:2], id+blobstore.PackExt)
	entries, err := w.Seal(finalPath)
	if err != nil {
		// Abort is safe after a failed Seal and a no-op after publish; it
		// removes the staging file this run instead of leaving it for the
		// next run's cleanStaging.
		_ = w.Abort()
		return fmt.Errorf("seal pack %s: %w", id, err)
	}
	rec := store.PackRecord{
		PackID:     id,
		EntryCount: int64(len(entries)),
		CreatedAt:  time.Now().UTC(),
	}
	indexEntries := make([]store.PackIndexEntry, 0, len(entries))
	for _, e := range entries {
		rec.StoredBytes += int64(e.StoredLen) //nolint:gosec // stored lengths are bounded by pack.MaxRawLen
		indexEntries = append(indexEntries, indexEntry(id, e))
	}
	if err := st.RecordPackedBlobs(rec, indexEntries); err != nil {
		return fmt.Errorf("record pack %s: %w", id, err)
	}
	stats.PacksSealed++
	slog.Info("sealed pack", "pack", id, "entries", len(entries), "storedBytes", rec.StoredBytes)
	for _, src := range sources {
		if err := os.Remove(src); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("remove packed source file", "path", src, "error", err)
			continue
		}
		// Best-effort parent cleanup; fails while non-empty.
		_ = os.Remove(filepath.Dir(src))
	}
	return nil
}

// indexEntry converts one kit pack entry to its store index row.
func indexEntry(packID string, e pack.Entry) store.PackIndexEntry {
	return store.PackIndexEntry{
		BlobHash:  e.ID.String(),
		PackID:    packID,
		Offset:    int64(e.Offset),    //nolint:gosec // pack offsets fit int64
		StoredLen: int64(e.StoredLen), //nolint:gosec // bounded by pack.MaxRawLen
		RawLen:    int64(e.RawLen),    //nolint:gosec // bounded by pack.MaxRawLen
		Flags:     uint8(e.Flags),
		CRC32C:    e.CRC32C,
	}
}

// sweepIndexed removes every loose file (outside the packs subtree) whose
// base name is an indexed blob hash: canonical leftovers from a crash between
// commit and delete, and noncanonical originals whose basename is the hash.
func sweepIndexed(st *store.Store, attachmentsDir, packsDir string, stats *Stats) error {
	indexed, err := st.ListIndexedBlobHashes()
	if err != nil {
		return err
	}
	if len(indexed) == 0 {
		return nil
	}
	var dirs []string
	err = filepath.WalkDir(attachmentsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == packsDir {
				return filepath.SkipDir
			}
			if path != attachmentsDir {
				dirs = append(dirs, path)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if _, ok := indexed[d.Name()]; !ok {
			return nil
		}
		//nolint:gosec // G122: the sweep removes the user's own loose files
		// under their attachments dir; the packer holds exclusive-writer
		// coverage, so there is no concurrent-writer TOCTOU to exploit.
		if err := os.Remove(path); err != nil {
			slog.Warn("sweep indexed loose file", "path", path, "error", err)
			return nil
		}
		stats.LooseSwept++
		return nil
	})
	if err != nil {
		return fmt.Errorf("sweep loose attachments: %w", err)
	}
	// Children are strictly longer than their parents, so deleting in
	// descending path-length order empties directories bottom-up.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
	return nil
}
