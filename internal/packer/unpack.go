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

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

type unpackPackReader interface {
	Entries() []pack.Entry
	ReadBlob(hash string, maxBytes int64) ([]byte, error)
	Close() error
}

// openUnpackPack is a narrow test seam for close-failure coverage. Production
// always uses the retained-descriptor bounded maintenance reader.
var openUnpackPack = func(path string) (unpackPackReader, error) {
	return blobstore.OpenMaintenancePack(path)
}

// storeRestoredAttachment is a narrow test seam around the durable loose-file
// authority boundary. Production always calls the export durable store.
var storeRestoredAttachment = export.StoreAttachmentFileDurable

// UnpackStats summarizes one unpack run.
type UnpackStats struct {
	PacksUnpacked  int   // packs restored and removed this run
	BlobsRestored  int   // blobs written back to canonical loose files
	BytesRestored  int64 // raw bytes written back
	MappingsPruned int   // unreferenced stale pack index rows removed
}

// Unpack restores every live packed blob to a canonical loose file through a
// bounded, hash-verifying maintenance reader. Only blobs with live index rows
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
	pruned, err := st.PruneUnreferencedPackIndex(ctx)
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
		return dropDeadPack(ctx, st, path, packID)
	}
	r, err := openUnpackPack(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("pack %s file is missing but %d blobs are indexed in it; "+
			"restore the pack file (e.g. from a backup) before unpacking", packID, len(liveEntries))
	}
	if err != nil {
		return fmt.Errorf("open pack %s: %w", packID, err)
	}
	planned, err := planRestoreEntries(ctx, r, liveEntries, packID)
	if err != nil {
		return errors.Join(err, closeUnpackPack(r, packID))
	}
	restored, rawBytes, restoreErr := restoreEntries(ctx, r, planned, attachmentsDir, packID)
	closeErr := closeUnpackPack(r, packID)
	if restoreErr != nil || closeErr != nil {
		return errors.Join(restoreErr, closeErr)
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

// planRestoreEntries validates the complete live index against the bounded
// reader's authoritative footer before the first loose write. Footer-only
// entries are dead and deliberately ignored.
func planRestoreEntries(ctx context.Context, r unpackPackReader, liveEntries []store.PackIndexEntry, packID string) ([]string, error) {
	footer := r.Entries()
	footerEntries := make(map[string]pack.Entry, len(footer))
	for _, e := range footer {
		if _, duplicate := footerEntries[e.ID.String()]; duplicate {
			return nil, fmt.Errorf("%w: duplicate blob %s in pack %s footer", pack.ErrCorrupt, e.ID, packID)
		}
		footerEntries[e.ID.String()] = e
	}
	planned := make([]string, 0, len(liveEntries))
	for _, live := range liveEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e, ok := footerEntries[live.BlobHash]
		if !ok {
			return nil, fmt.Errorf("%w: indexed blob %s is absent from pack %s footer",
				pack.ErrCorrupt, live.BlobHash, packID)
		}
		if live.PackID != packID || uint64(live.Offset) != e.Offset || //nolint:gosec // store scan rejects negative values
			uint64(live.StoredLen) != e.StoredLen || //nolint:gosec // store scan rejects negative values
			uint64(live.RawLen) != e.RawLen || //nolint:gosec // store scan rejects negative values
			pack.BlobFlags(live.Flags) != e.Flags || live.CRC32C != e.CRC32C {
			return nil, fmt.Errorf("%w: indexed metadata for blob %s does not match pack %s footer",
				pack.ErrCorrupt, live.BlobHash, packID)
		}
		if e.RawLen > uint64(maintenanceBlobBytes) { //nolint:gosec // positive fixed production/test limit
			return nil, fmt.Errorf("plan blob %s from pack %s: %w", live.BlobHash, packID,
				&blobstore.LimitError{Dimension: blobstore.LimitBlobRawBytes,
					Actual: e.RawLen, Limit: uint64(maintenanceBlobBytes)}) //nolint:gosec // positive fixed production/test limit
		}
		if e.StoredLen > uint64(maintenanceBlobBytes) { //nolint:gosec // positive fixed production/test limit
			return nil, fmt.Errorf("plan blob %s from pack %s: %w", live.BlobHash, packID,
				&blobstore.LimitError{Dimension: blobstore.LimitBlobStoredBytes,
					Actual: e.StoredLen, Limit: uint64(maintenanceBlobBytes)}) //nolint:gosec // positive fixed production/test limit
		}
		planned = append(planned, live.BlobHash)
	}
	return planned, nil
}

// restoreEntries writes a fully planned pack's live entries back to canonical
// loose files. Any read or write error (or a cancelled ctx) aborts the run
// with pack authority untouched, so already-restored loose copies are harmless.
func restoreEntries(ctx context.Context, r unpackPackReader, planned []string, attachmentsDir, packID string) (int, int64, error) {
	var restored int
	var rawBytes int64
	for _, hash := range planned {
		if err := ctx.Err(); err != nil {
			return restored, rawBytes, err
		}
		data, err := r.ReadBlob(hash, maintenanceBlobBytes)
		if err != nil {
			return restored, rawBytes, fmt.Errorf("read blob %s from pack %s: %w", hash, packID, err)
		}
		if err := restoreBlob(attachmentsDir, hash, data); err != nil {
			return restored, rawBytes, fmt.Errorf("restore blob %s from pack %s: %w", hash, packID, err)
		}
		restored++
		rawBytes += int64(len(data))
	}
	return restored, rawBytes, nil
}

func closeUnpackPack(r unpackPackReader, packID string) error {
	if err := r.Close(); err != nil {
		return fmt.Errorf("close pack %s reader before metadata removal: %w", packID, err)
	}
	return nil
}

// restoreBlob writes one blob to its canonical loose path through the durable
// export store. It returns only after the final regular no-follow descriptor
// and its parent directory have been synced, including for an empty blob.
func restoreBlob(attachmentsDir, hash string, data []byte) error {
	normalized, err := normalizeBlobHash(hash)
	if err != nil {
		return fmt.Errorf("restore attachment with invalid hash %q: %w", hash, err)
	}
	hash = normalized.String()
	att := &mime.Attachment{Content: data, ContentHash: hash}
	_, err = storeRestoredAttachment(attachmentsDir, att)
	return err
}

// dropDeadPack removes a zero-live pack without opening it. Dead packs may be
// corrupt (for example after orphan rescue), and their footer entries must not
// block downgrade or be resurrected as loose blobs.
func dropDeadPack(ctx context.Context, st *store.Store, path, packID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
