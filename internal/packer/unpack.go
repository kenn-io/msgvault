package packer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"

	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// UnpackStats summarizes one unpack run.
type UnpackStats struct {
	PacksUnpacked  int   // packs restored and removed this run
	BlobsRestored  int   // blobs written back to canonical loose files
	BytesRestored  int64 // raw bytes written back
	MappingsPruned int   // unreferenced stale pack index rows removed
}

// Unpack streams every live packed blob back to a canonical loose file
// (hash-verified by pack.Reader.ReadBlob). Only blobs with live index rows
// are restored; dead footer entries are ignored. It then drops the pack's
// index and attachment_packs rows and deletes the pack file. The caller must
// hold the archive's exclusive-writer coverage AND the daemon must not be
// running (its cached pack readers would hold deleted packs open).
func Unpack(ctx context.Context, st *store.Store, attachmentsDir string) (UnpackStats, error) {
	var stats UnpackStats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	attachmentsDir = filepath.Clean(attachmentsDir)
	packsDir := filepath.Join(attachmentsDir, "packs")
	pruned, err := st.PruneUnreferencedPackIndex()
	if err != nil {
		return stats, err
	}
	if pruned > int64(math.MaxInt) {
		return stats, fmt.Errorf("pruned mapping count %d exceeds platform int", pruned)
	}
	stats.MappingsPruned = int(pruned)
	recs, err := st.ListPackRecords()
	if err != nil {
		return stats, err
	}
	for _, rec := range recs {
		if err := unpackOne(ctx, st, attachmentsDir, packsDir, rec.PackID, &stats); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// unpackOne restores one pack's blobs to loose files, then drops its rows
// and file. Rows are dropped BEFORE the file delete: a crash between the
// two leaves an orphan pack that reconciliation re-adopts (or removes as
// redundant) — never an index row pointing at a missing pack, which would
// break reads. Cancellation is honored on entry, between blob restores, and
// before the record delete, always returning ctx.Err() without touching the
// DB; already-restored loose files are harmless (the pack stays authoritative
// and the packer's sweep removes indexed loose files).
func unpackOne(ctx context.Context, st *store.Store, attachmentsDir, packsDir, packID string, stats *UnpackStats) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !pack.IsValidPackID(packID) {
		return fmt.Errorf("attachment_packs row has malformed pack id %q", packID)
	}
	path := packFilePath(packsDir, packID)
	liveEntries, err := st.ListAttachmentPackEntries(packID)
	if err != nil {
		return err
	}
	if len(liveEntries) == 0 {
		return dropDeadPack(st, path, packID)
	}
	r, err := pack.OpenReader(path, nil)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("pack %s file is missing but %d blobs are indexed in it; "+
			"restore the pack file (e.g. from a backup) before unpacking", packID, len(liveEntries))
	}
	if err != nil {
		return fmt.Errorf("open pack %s: %w", packID, err)
	}
	restored, rawBytes, err := restoreEntries(ctx, r, liveEntries, attachmentsDir, packID)
	if closeErr := r.Close(); closeErr != nil {
		slog.Warn("close pack reader", "pack", packID, "error", closeErr)
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := st.DeletePackRecord(packID); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove unpacked pack file %s: %w", path, err)
	}
	// Best-effort shard dir cleanup; fails while non-empty.
	_ = os.Remove(filepath.Dir(path))
	stats.PacksUnpacked++
	stats.BlobsRestored += restored
	stats.BytesRestored += rawBytes
	slog.Info("unpacked pack", "pack", packID, "blobs", restored, "rawBytes", rawBytes)
	return nil
}

// restoreEntries writes every live index entry back to its canonical loose
// file, resolving the authoritative read metadata from the pack footer.
// Footer-only entries are dead and deliberately ignored. Any read or write
// error (or a cancelled ctx) aborts the run with the pack untouched, so no
// data can be lost.
func restoreEntries(ctx context.Context, r *pack.Reader, liveEntries []store.PackIndexEntry, attachmentsDir, packID string) (int, int64, error) {
	footerEntries := make(map[string]pack.Entry, len(r.Entries()))
	for _, e := range r.Entries() {
		footerEntries[e.ID.String()] = e
	}
	var restored int
	var rawBytes int64
	for _, live := range liveEntries {
		if err := ctx.Err(); err != nil {
			return restored, rawBytes, err
		}
		e, ok := footerEntries[live.BlobHash]
		if !ok {
			return restored, rawBytes, fmt.Errorf("indexed blob %s is absent from pack %s footer",
				live.BlobHash, packID)
		}
		data, err := r.ReadBlob(e)
		if err != nil {
			return restored, rawBytes, fmt.Errorf("read blob %s from pack %s: %w", e.ID, packID, err)
		}
		if err := restoreBlob(attachmentsDir, e.ID.String(), data); err != nil {
			return restored, rawBytes, fmt.Errorf("restore blob %s from pack %s: %w", e.ID, packID, err)
		}
		restored++
		rawBytes += int64(len(data))
	}
	return restored, rawBytes, nil
}

// restoreBlob writes one blob to its canonical loose path via the export
// store (atomic write, dedup, hash validation). StoreAttachmentFile skips
// empty content, so zero-length blobs are written directly.
func restoreBlob(attachmentsDir, hash string, data []byte) error {
	if len(data) == 0 {
		dir := filepath.Join(attachmentsDir, hash[:2])
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, hash), nil, 0o600)
	}
	att := &mime.Attachment{Content: data, ContentHash: hash}
	_, err := export.StoreAttachmentFile(attachmentsDir, att)
	return err
}

// dropDeadPack removes a zero-live pack without opening it. Dead packs may be
// corrupt (for example after orphan rescue), and their footer entries must not
// block downgrade or be resurrected as loose blobs.
func dropDeadPack(st *store.Store, path, packID string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove dead pack file %s: %w", path, err)
	}
	// With no live rows it is safe to remove the bytes first. A crash before
	// the record delete leaves only a harmless zero-live missing-pack record,
	// which the next unpack run drops through this same path.
	if err := st.DeletePackRecord(packID); err != nil {
		return err
	}
	_ = os.Remove(filepath.Dir(path))
	slog.Info("dropped pack with no live blobs", "pack", packID)
	return nil
}
