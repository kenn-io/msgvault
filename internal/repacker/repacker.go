// Package repacker reclaims dead bytes from immutable attachment pack files.
// It writes and seals replacement packs before atomically swapping live index
// rows, then retires old readers/files while retaining retryable inventory on
// any physical deletion failure.
package repacker

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
	"time"

	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/store"
)

const (
	minPackAge    = 24 * time.Hour
	minDeadStored = int64(8 << 20)
)

// Options tunes one repack run. Zero values select production defaults.
type Options struct {
	TargetSize int64
	MaxBytes   int64
	Now        time.Time
}

// Stats summarizes one repack run.
type Stats struct {
	MappingsPruned         int
	PacksSelected          int
	PacksRewritten         int
	PacksSealed            int
	PacksRemoved           int
	PacksDeferredOversized int
	BlobsRepacked          int
	BytesRepacked          int64
	BudgetExhausted        bool
}

// BlobStore is the production verified attachment read path plus daemon-cache
// reader retirement.
type BlobStore interface {
	ReadBounded(hash string, maxBytes int64) ([]byte, int64, error)
	RetirePack(packID string) error
}

type packWriter interface {
	ID() string
	Append(data []byte) (pack.Entry, error)
	Full() bool
	Seal(path string) ([]pack.Entry, error)
	Abort() error
}

// newPackWriter is the narrow failure-injection seam for otherwise
// unreachable kit writer failures. Production always installs pack.NewWriter.
var newPackWriter = func(stagingDir string, opts pack.WriterOptions) (packWriter, error) {
	return pack.NewWriter(stagingDir, opts)
}

type maintenancePackReader interface {
	Entries() []pack.Entry
	Close() error
}

var openMaintenancePack = func(path string) (maintenancePackReader, error) {
	return blobstore.OpenMaintenancePack(path)
}

// maxReplacementPackEntries is a test seam for the production maintenance
// footer-entry ceiling. Production never changes it.
var maxReplacementPackEntries = blobstore.MaxMaintenancePackEntries

type sourceEntry struct {
	oldPackID string
	entry     store.PackIndexEntry
}

// Run reclaims each selected source independently. Zero-live inventory is
// retired before any source content is opened, and an automatic run can make
// progress on healthy sources even when another pack is corrupt.
func Run(ctx context.Context, st *store.Store, blobs BlobStore, attachmentsDir string, opts Options) (Stats, error) {
	var stats Stats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if opts.TargetSize < 0 || opts.MaxBytes < 0 {
		return stats, errors.New("repack target size and byte budget must not be negative")
	}
	attachmentsDir = filepath.Clean(attachmentsDir)
	packsDir := filepath.Join(attachmentsDir, "packs")
	if err := os.MkdirAll(packsDir, 0o700); err != nil {
		return stats, fmt.Errorf("create attachment packs dir: %w", err)
	}

	pruned, err := st.PruneUnreferencedPackIndex(ctx)
	if err != nil {
		return stats, fmt.Errorf("prune unreferenced pack index before repack: %w", err)
	}
	if pruned > int64(math.MaxInt) {
		return stats, fmt.Errorf("pruned mapping count %d exceeds platform int", pruned)
	}
	stats.MappingsPruned = int(pruned)

	usage, err := st.ListPackUsage(ctx)
	if err != nil {
		return stats, err
	}
	selected, _ := selectPacks(usage, opts)
	stats.PacksSelected = len(selected)

	var runErr error
	partial := make([]store.PackUsage, 0, len(selected))
	for _, candidate := range selected {
		if candidate.LiveEntries != 0 {
			partial = append(partial, candidate)
			continue
		}
		if err := ctx.Err(); err != nil {
			return stats, errors.Join(runErr, err)
		}
		runErr = errors.Join(runErr, retireSource(ctx, st, blobs, candidate.PackID, &stats))
	}

	automatic := opts.MaxBytes > 0
	for _, candidate := range partial {
		if err := ctx.Err(); err != nil {
			return stats, errors.Join(runErr, err)
		}
		if automatic && stats.BytesRepacked >= opts.MaxBytes {
			stats.BudgetExhausted = true
			break
		}
		if limit := usageLimit(candidate); limit != nil {
			stats.PacksDeferredOversized++
			logPackLimit(candidate.PackID, limit)
			continue
		}
		entries, err := st.ListReferencedPackEntries(ctx, candidate.PackID)
		if err != nil {
			return stats, errors.Join(runErr, err)
		}
		if err := verifyUsageSnapshot(candidate, entries); err != nil {
			return stats, errors.Join(runErr, err)
		}
		preflightErr, closeErr := preflightSourcePack(packsDir, candidate.PackID, entries)
		if closeErr != nil {
			var contentErr error
			if preflightErr != nil {
				contentErr = fmt.Errorf("preflight attachment pack %s: %w", candidate.PackID, preflightErr)
			}
			return stats, errors.Join(runErr, contentErr,
				fmt.Errorf("close preflight attachment pack %s: %w", candidate.PackID, closeErr))
		}
		if err := preflightErr; err != nil {
			var limitErr *blobstore.LimitError
			if errors.As(err, &limitErr) {
				stats.PacksDeferredOversized++
				logPackLimit(candidate.PackID, limitErr)
				continue
			}
			contentErr := fmt.Errorf("preflight attachment pack %s: %w", candidate.PackID, err)
			if !automatic || !isKnownSourceContentError(err) {
				return stats, errors.Join(runErr, contentErr)
			}
			runErr = errors.Join(runErr, contentErr)
			continue
		}
		result, err := rewriteSource(ctx, blobs, packsDir, opts.TargetSize, candidate.PackID, entries)
		if err != nil {
			var limitErr *blobstore.LimitError
			if errors.As(err, &limitErr) {
				stats.PacksDeferredOversized++
				logPackLimit(candidate.PackID, limitErr)
				continue
			}
			var contentErr *sourceContentError
			if !errors.As(err, &contentErr) {
				return stats, errors.Join(runErr, err)
			}
			if !automatic {
				return stats, errors.Join(runErr, err)
			}
			runErr = errors.Join(runErr, err)
			continue
		}
		if err := ctx.Err(); err != nil {
			return stats, errors.Join(runErr, err)
		}
		if err := st.CommitRepack(ctx, []string{candidate.PackID}, result.records, result.moves); err != nil {
			return stats, errors.Join(runErr, fmt.Errorf("commit repacked attachment mappings for %s: %w", candidate.PackID, err))
		}
		stats.PacksRewritten++
		stats.PacksSealed += result.packsSealed
		stats.BlobsRepacked += result.blobsRepacked
		stats.BytesRepacked += result.bytesRepacked
		runErr = errors.Join(runErr, retireSource(ctx, st, blobs, candidate.PackID, &stats))
	}
	return stats, runErr
}

func selectPacks(usage []store.PackUsage, opts Options) ([]store.PackUsage, bool) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	ordered := append([]store.PackUsage(nil), usage...)
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].PackID < ordered[j].PackID
	})

	var zeroLive, partial []store.PackUsage
	for _, candidate := range ordered {
		if candidate.LiveEntries == 0 {
			zeroLive = append(zeroLive, candidate)
			continue
		}
		deadStored := candidate.StoredBytes - candidate.LiveStoredBytes
		belowHalf := candidate.EntryCount > 0 &&
			candidate.LiveEntries <= (candidate.EntryCount-1)/2
		oldEnough := !candidate.CreatedAt.After(now.Add(-minPackAge))
		if !belowHalf || !oldEnough || deadStored < minDeadStored {
			continue
		}
		partial = append(partial, candidate)
	}
	return append(zeroLive, partial...), false
}

func retireSource(ctx context.Context, st *store.Store, blobs BlobStore, packID string, stats *Stats) error {
	if err := blobs.RetirePack(packID); err != nil {
		return fmt.Errorf("retire old attachment pack %s: %w", packID, err)
	}
	deleted, err := st.DeleteEmptyPackRecord(ctx, packID)
	if err != nil {
		return fmt.Errorf("delete retired attachment pack record %s: %w", packID, err)
	}
	if !deleted {
		return fmt.Errorf("delete retired attachment pack record %s: pack still has referenced mappings or record is missing", packID)
	}
	stats.PacksRemoved++
	return nil
}

func usageLimit(candidate store.PackUsage) *blobstore.LimitError {
	if candidate.MaxLiveRawLen > blobstore.MaxMaintenanceBlobBytes {
		return &blobstore.LimitError{Dimension: blobstore.LimitBlobRawBytes,
			Actual: uint64(candidate.MaxLiveRawLen), Limit: blobstore.MaxMaintenanceBlobBytes}
	}
	if candidate.MaxLiveStoredLen > blobstore.MaxMaintenanceBlobBytes {
		return &blobstore.LimitError{Dimension: blobstore.LimitBlobStoredBytes,
			Actual: uint64(candidate.MaxLiveStoredLen), Limit: blobstore.MaxMaintenanceBlobBytes}
	}
	return nil
}

func logPackLimit(packID string, limit *blobstore.LimitError) {
	args := []any{"pack", packID}
	switch limit.Dimension {
	case blobstore.LimitBlobRawBytes:
		args = append(args, "raw_bytes", limit.Actual, "max_raw_bytes", limit.Limit)
	case blobstore.LimitBlobStoredBytes:
		args = append(args, "stored_bytes", limit.Actual, "max_stored_bytes", limit.Limit)
	default:
		args = append(args, "dimension", limit.Dimension, "actual", limit.Actual, "limit", limit.Limit)
	}
	slog.Warn("attachment pack exceeds maintenance ceiling; deferring repack", args...)
}

func verifyUsageSnapshot(candidate store.PackUsage, entries []store.PackIndexEntry) error {
	var stored, raw int64
	for _, entry := range entries {
		stored += entry.StoredLen
		raw += entry.RawLen
	}
	if int64(len(entries)) != candidate.LiveEntries || stored != candidate.LiveStoredBytes || raw != candidate.LiveRawBytes {
		return fmt.Errorf("pack %s referenced entries changed during repack selection: usage=(%d,%d,%d) entries=(%d,%d,%d)",
			candidate.PackID, candidate.LiveEntries, candidate.LiveStoredBytes, candidate.LiveRawBytes,
			len(entries), stored, raw)
	}
	return nil
}

func preflightSourcePack(packsDir, packID string, entries []store.PackIndexEntry) (error, error) {
	path := filepath.Join(packsDir, packID[:2], packID+blobstore.PackExt)
	reader, err := openMaintenancePack(path)
	if err != nil {
		return err, nil
	}
	footer := make(map[string]pack.Entry)
	for _, entry := range reader.Entries() {
		footer[entry.ID.String()] = entry
	}
	var checkErr error
	for _, indexed := range entries {
		authoritative, ok := footer[indexed.BlobHash]
		if !ok {
			checkErr = fmt.Errorf("%w: referenced blob %s is absent from pack footer", pack.ErrCorrupt, indexed.BlobHash)
			break
		}
		if authoritative.RawLen > blobstore.MaxMaintenanceBlobBytes {
			checkErr = &blobstore.LimitError{Dimension: blobstore.LimitBlobRawBytes,
				Actual: authoritative.RawLen, Limit: blobstore.MaxMaintenanceBlobBytes}
			break
		}
		if authoritative.StoredLen > blobstore.MaxMaintenanceBlobBytes {
			checkErr = &blobstore.LimitError{Dimension: blobstore.LimitBlobStoredBytes,
				Actual: authoritative.StoredLen, Limit: blobstore.MaxMaintenanceBlobBytes}
			break
		}
		if indexed.PackID != packID || uint64(indexed.Offset) != authoritative.Offset || //nolint:gosec // store scan validation rejects negative offsets
			uint64(indexed.StoredLen) != authoritative.StoredLen || //nolint:gosec // store scan validation rejects negative lengths
			uint64(indexed.RawLen) != authoritative.RawLen || //nolint:gosec // store scan validation rejects negative lengths
			pack.BlobFlags(indexed.Flags) != authoritative.Flags || indexed.CRC32C != authoritative.CRC32C {
			checkErr = fmt.Errorf("%w: indexed metadata for blob %s does not match pack footer", pack.ErrCorrupt, indexed.BlobHash)
			break
		}
	}
	return checkErr, reader.Close()
}

type sourceContentError struct{ err error }

func (e *sourceContentError) Error() string { return e.err.Error() }
func (e *sourceContentError) Unwrap() error { return e.err }

func isKnownSourceContentError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, fs.ErrPermission) {
		return false
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && !errors.Is(pathErr, fs.ErrNotExist) {
		return false
	}
	var syscallErr *os.SyscallError
	if errors.As(err, &syscallErr) {
		return false
	}
	for _, known := range []error{
		fs.ErrNotExist,
		pack.ErrBadMagic,
		pack.ErrUnsupportedVersion,
		pack.ErrTruncated,
		pack.ErrChecksum,
		pack.ErrCorrupt,
		pack.ErrBlobMismatch,
		pack.ErrEncrypted,
		pack.ErrDecrypt,
	} {
		if errors.Is(err, known) {
			return true
		}
	}
	var contentErr *sourceContentError
	return errors.As(err, &contentErr)
}

type rewriteResult struct {
	records       []store.PackRecord
	moves         []store.RepackMove
	packsSealed   int
	blobsRepacked int
	bytesRepacked int64
}

func rewriteSource(ctx context.Context, blobs BlobStore, packsDir string, targetSize int64, oldPackID string, entries []store.PackIndexEntry) (rewriteResult, error) {
	var result rewriteResult
	var writer packWriter
	var currentSources []sourceEntry
	abort := func(cause error, content bool) error {
		if writer != nil {
			if abortErr := writer.Abort(); abortErr != nil {
				return errors.Join(cause, fmt.Errorf("abort replacement attachment pack: %w", abortErr))
			}
		}
		if content {
			return &sourceContentError{err: cause}
		}
		return cause
	}
	seal := func() error {
		if writer == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return abort(err, false)
		}
		id := writer.ID()
		path := filepath.Join(packsDir, id[:2], id+blobstore.PackExt)
		entries, err := writer.Seal(path)
		if err != nil {
			return abort(fmt.Errorf("seal replacement attachment pack %s: %w", id, err), false)
		}
		if len(entries) != len(currentSources) {
			return fmt.Errorf("sealed replacement pack %s returned %d entries, want %d", id, len(entries), len(currentSources))
		}
		rec := store.PackRecord{
			PackID: id, EntryCount: int64(len(entries)), CreatedAt: time.Now().UTC(),
		}
		for i, entry := range entries {
			source := currentSources[i]
			if entry.ID.String() != source.entry.BlobHash {
				return fmt.Errorf("sealed replacement pack %s entry %d has blob %s, want %s", id, i, entry.ID, source.entry.BlobHash)
			}
			rec.StoredBytes += int64(entry.StoredLen) //nolint:gosec // kit bounds stored lengths
			result.moves = append(result.moves, store.RepackMove{
				OldPackID: source.oldPackID,
				NewEntry: store.PackIndexEntry{
					BlobHash: entry.ID.String(), PackID: id,
					Offset:    int64(entry.Offset),    //nolint:gosec // kit bounds valid pack offsets to representable files
					StoredLen: int64(entry.StoredLen), //nolint:gosec // kit bounds stored frames
					RawLen:    int64(entry.RawLen),    //nolint:gosec // kit bounds raw frames
					Flags:     uint8(entry.Flags), CRC32C: entry.CRC32C,
				},
			})
		}
		result.records = append(result.records, rec)
		result.packsSealed++
		writer = nil
		currentSources = nil
		return nil
	}

	for _, indexed := range entries {
		if err := ctx.Err(); err != nil {
			return result, abort(err, false)
		}
		data, reportedSize, err := blobs.ReadBounded(indexed.BlobHash, blobstore.MaxMaintenanceBlobBytes)
		if err != nil {
			content := isKnownSourceContentError(err)
			return result, abort(fmt.Errorf(
				"read live blob %s from source pack %s: %w",
				indexed.BlobHash, oldPackID, err), content)
		}
		if reportedSize != int64(len(data)) || indexed.RawLen != int64(len(data)) {
			return result, abort(fmt.Errorf(
				"live blob %s length mismatch: index=%d reader=%d actual=%d",
				indexed.BlobHash, indexed.RawLen, reportedSize, len(data)), true)
		}

		if writer != nil && len(currentSources) >= maxReplacementPackEntries {
			if err := seal(); err != nil {
				return result, err
			}
		}
		if writer == nil {
			writer, err = newPackWriter(packsDir, pack.WriterOptions{
				TargetSize: targetSize, ZstdLevel: pack.DefaultZstdLevel,
			})
			if err != nil {
				return result, fmt.Errorf("create replacement attachment pack: %w", err)
			}
		}
		entry, err := writer.Append(data)
		if err != nil {
			return result, abort(fmt.Errorf("append live blob %s to replacement pack: %w", indexed.BlobHash, err), false)
		}
		if entry.ID.String() != indexed.BlobHash {
			return result, abort(fmt.Errorf(
				"appended blob id %s does not match expected %s",
				entry.ID, indexed.BlobHash), true)
		}
		currentSources = append(currentSources, sourceEntry{oldPackID: oldPackID, entry: indexed})
		result.blobsRepacked++
		result.bytesRepacked += int64(len(data))
		if writer.Full() {
			if err := seal(); err != nil {
				return result, err
			}
		}
	}
	if err := seal(); err != nil {
		return result, err
	}
	return result, nil
}
