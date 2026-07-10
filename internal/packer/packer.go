// Package packer migrates loose content-addressed attachment blobs into
// sealed kit pack files under <attachmentsDir>/packs/, reconciling crash
// leftovers and sweeping indexed loose files. See
// docs/internal/packed-attachments-design.md.
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
	// MaxBytes is a soft raw-byte budget checked after each verified append;
	// 0 means unlimited.
	MaxBytes int64
}

// Stats summarizes one packer run.
type Stats struct {
	PacksSealed            int   // packs written this run
	BlobsPacked            int   // blobs appended this run
	BytesPacked            int64 // raw bytes appended this run
	PacksAdopted           int   // orphan packs adopted during reconciliation
	PacksRemoved           int   // fully-redundant orphan packs deleted
	PacksQuarantined       int   // readable orphans withheld after a referenced candidate failed verification
	PacksUnreadable        int   // orphan pack containers whose footer could not be opened
	RecordsDropped         int   // unusable pack records dropped so loose blobs can re-pack
	MappingsPruned         int   // unreferenced stale pack index rows removed
	BlobsMissing           int   // enumerated blobs whose file was missing (left for backfill)
	BlobsCorrupt           int   // files whose bytes did not match their recorded hash (skipped)
	BlobsDeferredOversized int   // verified blobs left loose because buffering would exceed the maintenance ceiling
	PacksDeferredOversized int   // orphan packs deferred because their container or an entry exceeds a maintenance ceiling
	LooseSwept             int   // indexed loose files removed by the sweep
	LooseOrphansRemoved    int   // unreferenced loose hash-named files removed
	BudgetExhausted        bool  // packing stopped after reaching the soft raw-byte budget
}

// removeLooseFile is a narrow failure-injection seam for best-effort orphan
// cleanup. Production always uses os.Remove.
var removeLooseFile = os.Remove

// Run packs all unindexed loose attachment blobs into sealed packs,
// reconciling crash leftovers first and sweeping indexed loose files after.
// The caller must hold the archive's exclusive-writer coverage (daemon
// operation gate or db.write.lock).
func Run(ctx context.Context, st *store.Store, attachmentsDir string, opts Options) (Stats, error) {
	var stats Stats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	attachmentsDir = filepath.Clean(attachmentsDir)
	packsDir := filepath.Join(attachmentsDir, "packs")
	if err := os.MkdirAll(packsDir, 0o700); err != nil {
		return stats, fmt.Errorf("create packs dir: %w", err)
	}
	if err := cleanStaging(ctx, packsDir); err != nil {
		return stats, err
	}
	if err := dropDanglingPackRecords(ctx, st, packsDir, &stats); err != nil {
		return stats, fmt.Errorf("drop dangling pack records: %w", err)
	}
	pruned, err := st.PruneUnreferencedPackIndex(ctx)
	if err != nil {
		return stats, fmt.Errorf("prune unreferenced pack index: %w", err)
	}
	if pruned > int64(math.MaxInt) {
		return stats, fmt.Errorf("pruned mapping count %d exceeds platform int", pruned)
	}
	stats.MappingsPruned = int(pruned)
	referenced, err := st.ListReferencedBlobHashes()
	if err != nil {
		return stats, err
	}
	if err := reconcilePacks(ctx, st, attachmentsDir, packsDir, referenced, &stats); err != nil {
		return stats, fmt.Errorf("reconcile orphan packs: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if err := packLoose(ctx, st, attachmentsDir, packsDir, opts, &stats); err != nil {
		return stats, err
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	indexed, err := st.ListIndexedBlobEntries()
	if err != nil {
		return stats, err
	}
	if err := sweepLoose(ctx, st, attachmentsDir, packsDir, referenced, indexed, &stats); err != nil {
		return stats, err
	}
	return stats, nil
}

// cleanStaging removes every *.staging file directly under packsDir. The
// packer runs exclusively, so any staging file is a dead mid-seal abort.
func cleanStaging(ctx context.Context, packsDir string) error {
	entries, err := os.ReadDir(packsDir)
	if err != nil {
		return fmt.Errorf("read packs dir: %w", err)
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".staging") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
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
func reconcilePacks(ctx context.Context, st *store.Store, attachmentsDir, packsDir string, referenced map[string]struct{}, stats *Stats) error {
	existingBlobs := blobstore.New(st, attachmentsDir)
	defer func() {
		if err := existingBlobs.Close(); err != nil {
			slog.Warn("close blob store after pack reconciliation", "error", err)
		}
	}()
	return filepath.WalkDir(packsDir, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
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
		if path != packFilePath(packsDir, id) {
			// Readers construct only the sharded path; adopting a mislocated
			// pack would index blobs the blob store cannot open (and the
			// sweep would then delete their loose copies).
			slog.Warn("skipping mislocated pack file; expected sharded path",
				"path", path, "expected", packFilePath(packsDir, id))
			return nil
		}
		has, err := st.HasPackRecord(id)
		if err != nil {
			return err
		}
		if has {
			return nil
		}
		return reconcileOnePack(ctx, st, existingBlobs, path, id, referenced, stats)
	})
}

// reconcileOnePack considers only attachment-referenced entries for adoption.
// It removes a pack whose entries are all dead or readable elsewhere, and
// adopts verified recovery candidates only when every referenced candidate in
// the pack verifies. One failure quarantines the whole orphan so a partial
// adoption cannot hide the damaged entry from future reconciliation.
func reconcileOnePack(ctx context.Context, st *store.Store, existingBlobs *blobstore.Store, path, id string, referenced map[string]struct{}, stats *Stats) error {
	r, err := blobstore.OpenMaintenancePack(path)
	if err != nil {
		if errors.Is(err, blobstore.ErrBlobTooLarge) {
			stats.PacksDeferredOversized++
			slog.Warn("orphan pack exceeds maintenance ceiling; deferring whole pack",
				"pack", id, "path", path, "limit", blobstore.MaxMaintenancePackBytes, "error", err)
			return nil
		}
		stats.PacksUnreadable++
		slog.Error("orphan pack container is unreadable; leaving pack in place",
			"pack", id, "path", path, "error", err)
		return nil
	}
	entries := r.Entries()
	if len(entries) > maintenancePackEntries {
		_ = r.Close()
		stats.PacksDeferredOversized++
		slog.Warn("orphan pack entry count exceeds maintenance ceiling; deferring whole pack",
			"pack", id, "path", path, "entries", len(entries), "limit", maintenancePackEntries)
		return nil
	}
	for _, entry := range entries {
		if entry.RawLen > uint64(maintenanceBlobBytes) || entry.StoredLen > uint64(maintenanceBlobBytes) { //nolint:gosec // maintenance limit is a positive fixed constant
			_ = r.Close()
			stats.PacksDeferredOversized++
			slog.Warn("orphan pack entry exceeds maintenance ceiling; deferring whole pack",
				"pack", id, "path", path, "hash", entry.ID.String(),
				"rawSize", entry.RawLen, "storedSize", entry.StoredLen, "limit", maintenanceBlobBytes)
			return nil
		}
	}
	adoptable, rec, failed, err := collectAdoptable(ctx, st, existingBlobs, r, id, referenced)
	if closeErr := r.Close(); closeErr != nil {
		slog.Warn("close orphan pack reader", "pack", id, "error", closeErr)
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if failed > 0 {
		stats.PacksQuarantined++
		slog.Error("orphan pack has damaged referenced recovery candidates; quarantining whole pack",
			"pack", id, "failedEntries", failed, "withheldEntries", len(adoptable))
		return nil
	}
	if len(adoptable) == 0 {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove redundant orphan pack %s: %w", id, err)
		}
		stats.PacksRemoved++
		slog.Info("removed fully-redundant orphan pack", "pack", id)
		return nil
	}
	if err := st.AdoptPackedBlobs(rec, adoptable); err != nil {
		return fmt.Errorf("adopt orphan pack %s: %w", id, err)
	}
	stats.PacksAdopted++
	slog.Info("adopted orphan pack", "pack", id, "entries", len(adoptable))
	return nil
}

// collectAdoptable returns referenced orphan entries that verify and either
// lack an index row or replace an unreadable packed copy. Unreferenced footer
// entries are dead and are not read. The PackRecord retains full immutable
// footer totals, while failed counts let the caller enforce all-or-nothing
// adoption for referenced recovery candidates.
func collectAdoptable(ctx context.Context, st *store.Store, existingBlobs *blobstore.Store, r *blobstore.MaintenancePackReader, id string, referenced map[string]struct{}) ([]store.PackIndexEntry, store.PackRecord, int, error) {
	entries := r.Entries()
	rec := store.PackRecord{
		PackID:     id,
		EntryCount: int64(len(entries)),
		CreatedAt:  time.Now().UTC(),
	}
	var adoptable []store.PackIndexEntry
	var failed int
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, rec, failed, err
		}
		rec.StoredBytes += int64(e.StoredLen) //nolint:gosec // stored lengths are bounded by pack.MaxRawLen
		hash := e.ID.String()
		if _, live := referenced[hash]; !live {
			continue
		}
		existing, err := st.GetAttachmentPackEntry(hash)
		if err != nil {
			return nil, rec, failed, err
		}
		if existing != nil && existing.PackID != id {
			_, _, readErr := existingBlobs.ReadBounded(hash, maintenanceBlobBytes)
			if readErr == nil {
				continue
			}
			slog.Error("existing packed blob is unreadable; adopting orphan replacement",
				"hash", hash, "existingPack", existing.PackID, "orphanPack", id, "error", readErr)
		}
		if _, err := r.ReadBlob(hash, maintenanceBlobBytes); err != nil {
			failed++
			slog.Warn("orphan pack entry failed verification; not adopting",
				"pack", id, "hash", hash, "error", err)
			continue
		}
		adoptable = append(adoptable, indexEntry(id, e))
	}
	return adoptable, rec, failed, nil
}

// dropDanglingPackRecords removes unusable attachment_packs records and their
// index rows before orphan reconciliation. That ordering matters when a
// missing recorded pack and an orphan pack contain the same blob: removing the
// stale index first lets reconciliation adopt the orphan instead of deleting
// it as redundant. Restored backups are the known producer of missing-pack
// records (restore materializes loose files, never production packs), while
// malformed pack IDs can only come from corrupt or manually edited metadata.
// Only a confirmed fs.ErrNotExist drops a valid record; a pack file that exists
// but cannot be read is left alone.
func dropDanglingPackRecords(ctx context.Context, st *store.Store, packsDir string, stats *Stats) error {
	recs, err := st.ListPackRecords()
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !pack.IsValidPackID(rec.PackID) {
			// Readers can never open a malformed pack ID, so retaining its index
			// rows would make packLoose skip readable loose copies and let the
			// final sweep delete them. Drop the unusable metadata first so normal
			// enumeration can recover any loose blobs.
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := st.DeletePackRecord(rec.PackID); err != nil {
				return err
			}
			stats.RecordsDropped++
			slog.Error("attachment_packs row has malformed pack id; dropped unusable metadata",
				"pack", rec.PackID)
			continue
		}
		_, statErr := os.Stat(packFilePath(packsDir, rec.PackID))
		if statErr == nil {
			continue
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("stat pack file for %s: %w", rec.PackID, statErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := st.DeletePackRecord(rec.PackID); err != nil {
			return err
		}
		stats.RecordsDropped++
		slog.Error("pack file missing; dropping its index rows so blobs re-pack from loose",
			"pack", rec.PackID)
	}
	return nil
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
		data, source, ok, err := readLooseBlob(ctx, st, attachmentsDir, b, stats)
		if err != nil {
			abort()
			return err
		}
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
		sources = append(sources, source)
		budgetExhausted := opts.MaxBytes > 0 && stats.BytesPacked >= opts.MaxBytes
		if w.Full() || len(sources) >= maintenancePackEntries || budgetExhausted {
			if err := ctx.Err(); err != nil {
				abort()
				return err
			}
			if err := sealAndCommit(st, packsDir, w, sources, stats); err != nil {
				return err
			}
			w = nil
			if budgetExhausted {
				stats.BudgetExhausted = true
				break
			}
		}
	}
	if w != nil {
		if err := ctx.Err(); err != nil {
			abort()
			return err
		}
		return sealAndCommit(st, packsDir, w, sources, stats)
	}
	return nil
}

// readLooseBlob loads and verifies one enumerated blob's bytes; a false
// return means the blob was skipped (counted in stats where applicable).
func readLooseBlob(ctx context.Context, st *store.Store, attachmentsDir string, b store.UnpackedBlob, stats *Stats) ([]byte, string, bool, error) {
	var sawMissing, sawCorrupt, sawOversized bool
	for _, path := range b.Paths {
		rel := filepath.FromSlash(path)
		if !filepath.IsLocal(rel) {
			slog.Warn("skipping blob candidate with non-local recorded path", "hash", b.Hash, "path", path)
			continue
		}
		full := filepath.Join(attachmentsDir, rel)
		info, statErr := os.Stat(full)
		if errors.Is(statErr, fs.ErrNotExist) {
			sawMissing = true
			slog.Warn("loose blob candidate missing", "hash", b.Hash, "path", path)
			continue
		}
		if statErr != nil {
			slog.Warn("skipping unreadable loose blob candidate", "hash", b.Hash, "path", path, "error", statErr)
			continue
		}
		if info.Size() > maintenanceBlobBytes {
			if err := canonicalizeLooseSource(ctx, st, attachmentsDir, b.Hash, full); err != nil {
				if errors.Is(err, errLooseHashMismatch) {
					sawCorrupt = true
					slog.Error("oversized loose blob candidate bytes do not match recorded hash",
						"hash", b.Hash, "path", path, "size", info.Size(), "limit", maintenanceBlobBytes)
					continue
				}
				if errors.Is(err, fs.ErrNotExist) {
					sawMissing = true
					continue
				}
				return nil, "", false, err
			}
			sawOversized = true
			slog.Warn("loose blob exceeds maintenance ceiling; leaving verified canonical copy loose",
				"hash", b.Hash, "path", path, "size", info.Size(), "limit", maintenanceBlobBytes)
			break
		}
		data, _, err := readVerifiedLoose(full, b.Hash, maintenanceBlobBytes)
		if errors.Is(err, blobstore.ErrBlobTooLarge) {
			// The descriptor grew after stat. Handle it through the same streaming
			// canonicalization path without allocating the new size.
			if err := canonicalizeLooseSource(ctx, st, attachmentsDir, b.Hash, full); err != nil {
				if errors.Is(err, errLooseHashMismatch) {
					sawCorrupt = true
					continue
				}
				return nil, "", false, err
			}
			sawOversized = true
			break
		}
		if err != nil {
			if errors.Is(err, errLooseHashMismatch) {
				sawCorrupt = true
				slog.Error("loose blob candidate bytes do not match recorded hash",
					"hash", b.Hash, "path", path)
				continue
			}
			slog.Warn("skipping unreadable loose blob candidate", "hash", b.Hash, "path", path, "error", err)
			continue
		}
		return data, full, true, nil
	}
	if sawOversized {
		stats.BlobsDeferredOversized++
	} else if sawCorrupt {
		stats.BlobsCorrupt++
	} else if sawMissing {
		stats.BlobsMissing++
	}
	return nil, "", false, nil
}

// sealAndCommit seals the writer to its final sharded path, records the pack
// and its index rows (canonicalizing recorded paths in the same transaction),
// and removes the packed source files. If recording fails, the sealed pack is
// adopted by reconciliation on the next run (design crash boundary).
func sealAndCommit(st *store.Store, packsDir string, w *pack.Writer, sources []string, stats *Stats) error {
	id := w.ID()
	finalPath := packFilePath(packsDir, id)
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

// packFilePath returns a pack's sharded final path under packsDir.
func packFilePath(packsDir, packID string) string {
	return filepath.Join(packsDir, packID[:2], packID+blobstore.PackExt)
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

// sweepLoose classifies every regular hash-named loose file outside packs/.
// Unreferenced files are best-effort garbage; referenced/indexed files retain
// the verified sweep/recovery behavior; referenced/unindexed files stay loose.
func sweepLoose(ctx context.Context, st *store.Store, attachmentsDir, packsDir string, referenced map[string]struct{}, indexed map[string]store.PackIndexEntry, stats *Stats) error {
	packed := blobstore.New(st, attachmentsDir)
	defer func() {
		if err := packed.Close(); err != nil {
			slog.Warn("close blob store after loose sweep", "error", err)
		}
	}()
	var dirs []string
	err := filepath.WalkDir(attachmentsDir, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
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
		if !isBlobHashName(d.Name()) {
			return nil
		}
		if _, live := referenced[d.Name()]; !live {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := removeLooseFile(path); err != nil {
				slog.Warn("remove unreferenced loose attachment", "path", path, "error", err)
				return nil
			}
			stats.LooseOrphansRemoved++
			return nil
		}
		entry, ok := indexed[d.Name()]
		if !ok {
			canonical := canonicalLoosePath(attachmentsDir, d.Name())
			if filepath.Clean(path) == filepath.Clean(canonical) {
				return nil
			}
			// A committed noncanonical migration can leave its source behind when
			// best-effort removal fails. Retry only after the canonical copy verifies.
			if verifyErr := verifyLooseStream(ctx, canonical, d.Name()); verifyErr != nil {
				return nil //nolint:nilerr // unverifiable canonical bytes deliberately preserve the legacy source
			}
			if err := removeLooseFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("retry legacy loose blob cleanup", "hash", d.Name(), "path", path, "error", err)
				return nil
			}
			stats.LooseSwept++
			return nil
		}
		// An index row alone is not proof that the packed copy is readable:
		// restored/corrupt metadata or damaged pack bytes can make blobstore
		// reads fail. Verify through the production read path before deleting a
		// loose recovery copy.
		_, _, err = packed.ReadBounded(d.Name(), maintenanceBlobBytes)
		if err != nil {
			slog.Error("packed copy is unreadable; validating loose recovery candidate",
				"path", path, "hash", d.Name(), "pack", entry.PackID, "error", err)
			if recoverErr := canonicalizeLooseSource(ctx, st, attachmentsDir, d.Name(), path); recoverErr != nil {
				var storeErr *canonicalizeStoreError
				if errors.As(recoverErr, &storeErr) || errors.Is(recoverErr, context.Canceled) ||
					errors.Is(recoverErr, context.DeadlineExceeded) {
					return recoverErr
				}
				slog.Error("preserving packed index because loose recovery candidate is unreadable",
					"path", path, "hash", d.Name(), "error", recoverErr)
				return nil
			}
			if err := st.DeletePackIndexEntry(d.Name()); err != nil {
				return err
			}
			delete(indexed, d.Name())
			slog.Error("dropped unreadable packed index after verifying loose recovery copy",
				"path", path, "hash", d.Name())
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
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

func isBlobHashName(name string) bool {
	_, err := pack.ParseBlobID(name)
	return err == nil
}
