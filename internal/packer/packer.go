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
	PacksSealed            int   // packs committed to pack metadata this run
	BlobsPacked            int   // blobs committed to pack metadata this run
	BytesPacked            int64 // raw bytes committed to pack metadata this run
	PacksAdopted           int   // orphan packs adopted during reconciliation
	PacksRemoved           int   // fully-redundant orphan packs deleted
	PacksQuarantined       int   // readable orphans withheld after a referenced candidate failed verification
	PacksUnreadable        int   // orphan pack containers whose footer could not be opened
	RecordsDropped         int   // unusable pack records dropped so loose blobs can re-pack
	MappingsPruned         int   // unreferenced stale pack index rows removed
	BlobsMissing           int   // enumerated blobs whose file was missing (left for backfill)
	BlobsCorrupt           int   // files whose bytes did not match their recorded hash (skipped)
	BlobsDeferredOversized int   // blobs left loose because buffering would exceed the maintenance ceiling
	PacksDeferredOversized int   // orphan packs deferred because their container or an entry exceeds a maintenance ceiling
	LooseSwept             int   // indexed loose files removed by the sweep
	LooseOrphansRemoved    int   // unreferenced loose hash-named files removed
	BudgetExhausted        bool  // packing stopped after reaching the soft raw-byte budget
}

// removeLooseFile is a narrow failure-injection seam for best-effort orphan
// cleanup. Production always uses os.Remove.
var removeLooseFile = os.Remove

// readExistingBounded is a narrow ordering seam for orphan-planning tests.
// Production always delegates to the bounded blob store.
var readExistingBounded = func(blobs *blobstore.Store, hash string, limit int64) ([]byte, int64, error) {
	return blobs.ReadBounded(hash, limit)
}

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
	rawReferenced, err := st.ListReferencedBlobHashes()
	if err != nil {
		return stats, err
	}
	referenced, malformedReferenced := normalizeReferencedInventory(rawReferenced)
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
	rawIndexed, err := st.ListIndexedBlobEntries()
	if err != nil {
		return stats, err
	}
	indexed, malformedIndexed := normalizeIndexedInventory(rawIndexed)
	if err := sweepLoose(ctx, st, attachmentsDir, packsDir, referenced, indexed,
		malformedReferenced || malformedIndexed, &stats); err != nil {
		return stats, err
	}
	return stats, nil
}

type referencedAliases map[normalizedBlobHash][]string

func normalizeReferencedInventory(raw map[string]struct{}) (referencedAliases, bool) {
	normalized := make(referencedAliases, len(raw))
	malformed := false
	for original := range raw {
		hash, err := normalizeBlobHash(original)
		if err != nil {
			malformed = true
			slog.Error("malformed referenced attachment hash; suppressing loose orphan deletion",
				"original_hash", original, "error", err)
			continue
		}
		normalized[hash] = append(normalized[hash], original)
	}
	for hash := range normalized {
		sort.Strings(normalized[hash])
	}
	return normalized, malformed
}

func normalizeIndexedInventory(raw map[string]store.PackIndexEntry) (map[normalizedBlobHash]store.PackIndexEntry, bool) {
	normalized := make(map[normalizedBlobHash]store.PackIndexEntry, len(raw))
	malformed := false
	for original, entry := range raw {
		hash, err := normalizeBlobHash(original)
		if err != nil {
			malformed = true
			slog.Error("malformed packed attachment hash; suppressing loose orphan deletion",
				"original_hash", original, "error", err)
			continue
		}
		if _, duplicate := normalized[hash]; duplicate {
			malformed = true
			slog.Error("case-colliding packed attachment hashes; suppressing loose orphan deletion",
				"original_hash", original, "hash", hash.String())
			continue
		}
		normalized[hash] = entry
	}
	return normalized, malformed
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
func reconcilePacks(ctx context.Context, st *store.Store, attachmentsDir, packsDir string, referenced referencedAliases, stats *Stats) error {
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
func reconcileOnePack(ctx context.Context, st *store.Store, existingBlobs *blobstore.Store, path, id string, referenced referencedAliases, stats *Stats) error {
	r, err := blobstore.OpenMaintenancePack(path)
	if err != nil {
		var limitErr *blobstore.LimitError
		if errors.As(err, &limitErr) {
			stats.PacksDeferredOversized++
			logOrphanLimitDeferral(id, limitErr, 0)
			return nil
		}
		if errors.Is(err, blobstore.ErrBlobTooLarge) {
			return fmt.Errorf("orphan pack %s returned an unclassified maintenance limit: %w", id, err)
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
		logOrphanLimitDeferral(id, &blobstore.LimitError{
			Dimension: blobstore.LimitPackEntryCount,
			Actual:    uint64(len(entries)),
			Limit:     uint64(maintenancePackEntries), //nolint:gosec // positive fixed production/test limit
		}, 0)
		return nil
	}
	collection, err := collectAdoptable(ctx, st, existingBlobs, r, id, referenced)
	if closeErr := r.Close(); closeErr != nil {
		slog.Warn("close orphan pack reader", "pack", id, "error", closeErr)
	}
	if err != nil {
		return err
	}
	if collection.deferred {
		stats.PacksDeferredOversized++
		logOrphanLimitDeferral(id, collection.limit, collection.withheld)
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if collection.failed > 0 {
		stats.PacksQuarantined++
		slog.Error("orphan pack has damaged referenced recovery candidates; quarantining whole pack",
			"pack", id, "failedEntries", collection.failed, "withheldEntries", len(collection.adoptable))
		return nil
	}
	if len(collection.adoptable) == 0 {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove redundant orphan pack %s: %w", id, err)
		}
		stats.PacksRemoved++
		slog.Info("removed fully-redundant orphan pack", "pack", id)
		return nil
	}
	if err := st.AdoptPackedBlobsWithAliases(collection.record, collection.adoptable); err != nil {
		return fmt.Errorf("adopt orphan pack %s: %w", id, err)
	}
	stats.PacksAdopted++
	slog.Info("adopted orphan pack", "pack", id, "entries", len(collection.adoptable))
	return nil
}

type orphanCollection struct {
	adoptable []store.PackIndexAdoption
	record    store.PackRecord
	failed    int
	limit     *blobstore.LimitError
	withheld  int
	deferred  bool
}

type orphanCandidatePlan struct {
	entry    pack.Entry
	existing *store.PackIndexEntry
	aliases  []string
}

// collectAdoptable plans every referenced candidate before reading any of
// them, then verifies candidates that lack a readable authoritative copy.
// Unreferenced footer entries are dead and are never size-gated or read. The
// PackRecord retains full immutable footer totals, while failure and deferral
// state lets the caller enforce all-or-nothing adoption.
func collectAdoptable(ctx context.Context, st *store.Store, existingBlobs *blobstore.Store, r *blobstore.MaintenancePackReader, id string, referenced referencedAliases) (orphanCollection, error) {
	entries := r.Entries()
	result := orphanCollection{record: store.PackRecord{
		PackID:     id,
		EntryCount: int64(len(entries)),
		CreatedAt:  time.Now().UTC(),
	}}
	var plans []orphanCandidatePlan
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.record.StoredBytes += int64(e.StoredLen) //nolint:gosec // stored lengths are bounded by pack.MaxRawLen
		hash, err := normalizeBlobHash(e.ID.String())
		if err != nil {
			return result, fmt.Errorf("normalize orphan footer blob id: %w", err)
		}
		aliases, live := referenced[hash]
		if !live {
			continue
		}
		result.withheld++
		existing, err := st.GetAttachmentPackEntry(hash.String())
		if err != nil {
			return result, err
		}
		plans = append(plans, orphanCandidatePlan{entry: e, existing: existing, aliases: aliases})
		maintenanceLimit := uint64(maintenanceBlobBytes) //nolint:gosec // positive fixed production/test limit
		if e.RawLen > maintenanceLimit {
			if result.limit == nil || e.RawLen > result.limit.Actual {
				result.limit = &blobstore.LimitError{
					Dimension: blobstore.LimitBlobRawBytes, Actual: e.RawLen, Limit: maintenanceLimit,
				}
			}
			result.deferred = true
		} else if e.StoredLen > maintenanceLimit {
			if result.limit == nil || e.StoredLen > result.limit.Actual {
				result.limit = &blobstore.LimitError{
					Dimension: blobstore.LimitBlobStoredBytes, Actual: e.StoredLen, Limit: maintenanceLimit,
				}
			}
			result.deferred = true
		}
	}
	if result.deferred {
		return result, nil
	}

	var candidates []orphanCandidatePlan
	for _, plan := range plans {
		e := plan.entry
		hash, err := normalizeBlobHash(e.ID.String())
		if err != nil {
			return result, fmt.Errorf("normalize planned orphan blob id: %w", err)
		}
		existing := plan.existing
		if existing != nil && existing.PackID != id {
			_, _, readErr := readExistingBounded(existingBlobs, hash.String(), maintenanceBlobBytes)
			if readErr == nil {
				continue
			}
			var limitErr *blobstore.LimitError
			if errors.As(readErr, &limitErr) {
				result.limit = limitErr
				result.deferred = true
				return result, nil
			}
			if errors.Is(readErr, blobstore.ErrBlobTooLarge) {
				return result, fmt.Errorf("existing packed blob %s returned an unclassified maintenance limit: %w", hash, readErr)
			}
			slog.Error("existing packed blob is unreadable; adopting orphan replacement",
				"hash", hash, "existingPack", existing.PackID, "orphanPack", id, "error", readErr)
		}
		candidates = append(candidates, plan)
	}

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		e := candidate.entry
		hash, err := normalizeBlobHash(e.ID.String())
		if err != nil {
			return result, fmt.Errorf("normalize orphan candidate blob id: %w", err)
		}
		if _, err := r.ReadBlob(hash.String(), maintenanceBlobBytes); err != nil {
			result.failed++
			slog.Warn("orphan pack entry failed verification; not adopting",
				"pack", id, "hash", hash, "error", err)
			continue
		}
		result.adoptable = append(result.adoptable, store.PackIndexAdoption{
			Entry:          indexEntry(id, e),
			OriginalHashes: candidate.aliases,
		})
	}
	return result, nil
}

func logOrphanLimitDeferral(packID string, limit *blobstore.LimitError, withheld int) {
	args := []any{
		"pack", packID,
		"limit_dimension", limit.Dimension,
		"withheld_entries", withheld,
	}
	switch limit.Dimension {
	case blobstore.LimitBlobRawBytes:
		args = append(args, "raw_bytes", limit.Actual, "max_raw_bytes", limit.Limit)
	case blobstore.LimitBlobStoredBytes:
		args = append(args, "stored_bytes", limit.Actual, "max_stored_bytes", limit.Limit)
	case blobstore.LimitBlobStatBytes:
		args = append(args, "observed_bytes", limit.Actual, "stat_bytes", limit.Limit)
	case blobstore.LimitPackContainerBytes:
		args = append(args, "pack_bytes", limit.Actual, "max_pack_bytes", limit.Limit)
	case blobstore.LimitPackFooterBytes:
		args = append(args, "footer_bytes", limit.Actual, "max_footer_bytes", limit.Limit)
	case blobstore.LimitPackEntryCount:
		args = append(args, "pack_entries", limit.Actual, "max_pack_entries", limit.Limit)
	}
	slog.Warn("orphan pack exceeds a maintenance limit; deferring whole pack", args...)
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
	var sources []packedLooseSource
	var pendingRawBytes int64
	appended := make(map[normalizedBlobHash]struct{}, len(blobs))
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
		hash, err := normalizeBlobHash(b.Hash)
		if err != nil {
			abort()
			return fmt.Errorf("enumerated unpacked blob %q became malformed after verification: %w", b.Hash, err)
		}
		if _, duplicate := appended[hash]; duplicate {
			abort()
			return fmt.Errorf("refusing duplicate normalized loose blob %s in one packing run", hash.String())
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
		appended[hash] = struct{}{}
		pendingRawBytes += int64(len(data))
		aliases := append([]string(nil), b.OriginalHashes...)
		if len(aliases) == 0 {
			aliases = []string{b.Hash}
		}
		sources = append(sources, packedLooseSource{path: source, originalHashes: aliases})
		budgetExhausted := opts.MaxBytes > 0 &&
			(stats.BytesPacked >= opts.MaxBytes || pendingRawBytes >= opts.MaxBytes-stats.BytesPacked)
		if w.Full() || len(sources) >= maintenancePackEntries || budgetExhausted {
			if err := ctx.Err(); err != nil {
				abort()
				return err
			}
			if err := sealAndCommit(st, packsDir, w, sources, stats); err != nil {
				return err
			}
			w = nil
			pendingRawBytes = 0
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

type packedLooseSource struct {
	path           string
	originalHashes []string
}

// readLooseBlob loads and verifies one enumerated blob's bytes; a false
// return means the blob was skipped (counted in stats where applicable).
func readLooseBlob(ctx context.Context, st *store.Store, attachmentsDir string, b store.UnpackedBlob, stats *Stats) ([]byte, string, bool, error) {
	hash, err := normalizeBlobHash(b.Hash)
	if err != nil {
		slog.Error("malformed unpacked attachment hash; preserving recorded candidates",
			"original_hash", b.Hash, "error", err)
		return nil, "", false, nil
	}
	var sawMissing, sawCorrupt, sawOversized bool
	paths := append([]string(nil), b.Paths...)
	canonicalRel := canonicalLooseRel(hash)
	canonicalListed := false
	for _, path := range paths {
		if filepath.Clean(filepath.FromSlash(path)) == filepath.Clean(filepath.FromSlash(canonicalRel)) {
			canonicalListed = true
			break
		}
	}
	hasNoncanonicalAlias := false
	for _, original := range b.OriginalHashes {
		if original != hash.String() {
			hasNoncanonicalAlias = true
			break
		}
	}
	if hasNoncanonicalAlias && !canonicalListed {
		paths = append(paths, canonicalRel)
	}
	for _, path := range paths {
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
			if isCanonicalLoosePath(attachmentsDir, hash, full) {
				if hasNoncanonicalAlias {
					if err := canonicalizeExistingLooseAliases(st, b, hash, full); err != nil {
						return nil, "", false, err
					}
				}
				sawOversized = true
				slog.Warn("loose blob exceeds maintenance ceiling; leaving canonical copy loose",
					"hash", hash.String(), "original_hash", b.Hash,
					"raw_bytes", info.Size(), "max_raw_bytes", maintenanceBlobBytes)
				break
			}
			if err := canonicalizeLooseSource(ctx, st, attachmentsDir, unpackedBlobAliases(b), hash, full); err != nil {
				if errors.Is(err, errLooseHashMismatch) {
					sawCorrupt = true
					slog.Error("oversized loose blob candidate bytes do not match recorded hash",
						"hash", hash.String(), "original_hash", b.Hash,
						"raw_bytes", info.Size(), "max_raw_bytes", maintenanceBlobBytes)
					continue
				}
				if errors.Is(err, fs.ErrNotExist) {
					sawMissing = true
					continue
				}
				return nil, "", false, err
			}
			sawOversized = true
			slog.Warn("loose blob exceeds maintenance ceiling; leaving migrated canonical copy loose",
				"hash", hash.String(), "original_hash", b.Hash,
				"raw_bytes", info.Size(), "max_raw_bytes", maintenanceBlobBytes)
			break
		}
		data, observedSize, err := readVerifiedLoose(full, hash, maintenanceBlobBytes)
		if errors.Is(err, blobstore.ErrBlobTooLarge) {
			// The descriptor grew after stat. Handle it through the same streaming
			// canonicalization path without allocating the new size.
			if isCanonicalLoosePath(attachmentsDir, hash, full) {
				if hasNoncanonicalAlias {
					if err := canonicalizeExistingLooseAliases(st, b, hash, full); err != nil {
						return nil, "", false, err
					}
				}
				sawOversized = true
				slog.Warn("loose blob grew beyond maintenance ceiling; leaving canonical copy loose",
					"hash", hash.String(), "original_hash", b.Hash,
					"raw_bytes", observedSize, "max_raw_bytes", maintenanceBlobBytes)
				break
			}
			if err := canonicalizeLooseSource(ctx, st, attachmentsDir, unpackedBlobAliases(b), hash, full); err != nil {
				if errors.Is(err, errLooseHashMismatch) {
					sawCorrupt = true
					continue
				}
				return nil, "", false, err
			}
			sawOversized = true
			slog.Warn("loose blob grew beyond maintenance ceiling; leaving migrated canonical copy loose",
				"hash", hash.String(), "original_hash", b.Hash,
				"raw_bytes", observedSize, "max_raw_bytes", maintenanceBlobBytes)
			break
		}
		if err != nil {
			if errors.Is(err, errLooseHashMismatch) {
				sawCorrupt = true
				slog.Error("loose blob candidate bytes do not match recorded hash",
					"hash", hash.String(), "original_hash", b.Hash, "path", path)
				continue
			}
			slog.Warn("skipping unreadable loose blob candidate", "hash", b.Hash, "path", path, "error", err)
			continue
		}
		if b.Hash != hash.String() {
			if err := canonicalizeLooseSource(ctx, st, attachmentsDir, unpackedBlobAliases(b), hash, full); err != nil {
				return nil, "", false, err
			}
			full = canonicalLoosePath(attachmentsDir, hash)
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

func unpackedBlobAliases(b store.UnpackedBlob) []string {
	if len(b.OriginalHashes) == 0 {
		return []string{b.Hash}
	}
	return b.OriginalHashes
}

func canonicalizeExistingLooseAliases(st *store.Store, b store.UnpackedBlob, hash normalizedBlobHash, canonical string) error {
	if err := validateCanonicalLooseObject(canonical); err != nil {
		return err
	}
	if err := pack.SyncDir(filepath.Dir(canonical)); err != nil {
		return fmt.Errorf("sync canonical loose directory before hash normalization: %w", err)
	}
	if err := st.CanonicalizeAttachmentBlobAliases(hash.String(), unpackedBlobAliases(b)); err != nil {
		return fmt.Errorf("canonicalize existing loose aliases for %s: %w", hash.String(), err)
	}
	return nil
}

// sealAndCommit seals the writer to its final sharded path, records the pack
// and its index rows (canonicalizing recorded paths in the same transaction),
// and removes the packed source files. If recording fails, the sealed pack is
// adopted by reconciliation on the next run (design crash boundary).
func sealAndCommit(st *store.Store, packsDir string, w *pack.Writer, sources []packedLooseSource, stats *Stats) error {
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
	if len(entries) != len(sources) {
		return fmt.Errorf("sealed pack %s returned %d entries for %d loose sources",
			id, len(entries), len(sources))
	}
	adoptions := make([]store.PackIndexAdoption, 0, len(entries))
	var rawBytes int64
	for i, e := range entries {
		rec.StoredBytes += int64(e.StoredLen) //nolint:gosec // stored lengths are bounded by pack.MaxRawLen
		rawBytes += int64(e.RawLen)           //nolint:gosec // raw lengths are bounded by pack.MaxRawLen
		adoptions = append(adoptions, store.PackIndexAdoption{
			Entry:          indexEntry(id, e),
			OriginalHashes: sources[i].originalHashes,
		})
	}
	if err := st.RecordPackedBlobsWithAliases(rec, adoptions); err != nil {
		return fmt.Errorf("record pack %s: %w", id, err)
	}
	stats.PacksSealed++
	stats.BlobsPacked += len(entries)
	stats.BytesPacked += rawBytes
	slog.Info("sealed pack", "pack", id, "entries", len(entries), "storedBytes", rec.StoredBytes)
	for _, src := range sources {
		if err := os.Remove(src.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("remove packed source file", "path", src.path, "error", err)
			continue
		}
		// Best-effort parent cleanup; fails while non-empty.
		_ = os.Remove(filepath.Dir(src.path))
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
func sweepLoose(ctx context.Context, st *store.Store, attachmentsDir, packsDir string,
	referenced referencedAliases, indexed map[normalizedBlobHash]store.PackIndexEntry,
	suppressOrphanDeletion bool, stats *Stats,
) error {
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
		hash, hashErr := normalizeBlobHash(d.Name())
		if hashErr != nil {
			slog.Error("malformed loose attachment filename; preserving file", "path", path, "error", hashErr)
			return nil
		}
		if _, live := referenced[hash]; !live {
			if suppressOrphanDeletion {
				return nil
			}
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
		entry, ok := indexed[hash]
		if !ok {
			canonical := canonicalLoosePath(attachmentsDir, hash)
			if filepath.Clean(path) == filepath.Clean(canonical) {
				return nil
			}
			// A committed noncanonical migration can leave its source behind when
			// best-effort removal fails. Retry only after the canonical copy verifies.
			if verifyErr := verifyLooseStream(ctx, canonical, hash); verifyErr != nil {
				return nil //nolint:nilerr // unverifiable canonical bytes deliberately preserve the legacy source
			}
			if err := pack.SyncDir(filepath.Dir(canonical)); err != nil {
				slog.Warn("preserve legacy loose blob after canonical directory sync failure",
					"hash", hash.String(), "path", path, "error", err)
				return nil
			}
			removed, removeErr := removeIndependentLoose(path, canonical)
			if removeErr != nil {
				slog.Warn("retry legacy loose blob cleanup", "hash", hash.String(), "path", path, "error", removeErr)
				return nil
			}
			if removed {
				stats.LooseSwept++
			}
			return nil
		}
		// An index row alone is not proof that the packed copy is readable:
		// restored/corrupt metadata or damaged pack bytes can make blobstore
		// reads fail. Verify through the production read path before deleting a
		// loose recovery copy.
		_, _, err = packed.ReadBounded(hash.String(), maintenanceBlobBytes)
		if err != nil {
			slog.Error("packed copy is unreadable; validating loose recovery candidate",
				"path", path, "hash", d.Name(), "pack", entry.PackID, "error", err)
			if recoverErr := canonicalizeLooseSource(ctx, st, attachmentsDir, []string{d.Name()}, hash, path); recoverErr != nil {
				var storeErr *canonicalizeStoreError
				if errors.As(recoverErr, &storeErr) || errors.Is(recoverErr, context.Canceled) ||
					errors.Is(recoverErr, context.DeadlineExceeded) {
					return recoverErr
				}
				slog.Error("preserving packed index because loose recovery candidate is unreadable",
					"path", path, "hash", d.Name(), "error", recoverErr)
				return nil
			}
			if err := st.DeletePackIndexEntry(hash.String()); err != nil {
				return err
			}
			delete(indexed, hash)
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
