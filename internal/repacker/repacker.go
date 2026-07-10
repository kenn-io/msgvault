// Package repacker reclaims dead bytes from immutable attachment pack files.
// It writes and seals replacement packs before atomically swapping live index
// rows, then retires old readers/files while retaining retryable inventory on
// any physical deletion failure.
package repacker

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	MappingsPruned  int
	PacksSelected   int
	PacksRewritten  int
	PacksSealed     int
	PacksRemoved    int
	BlobsRepacked   int
	BytesRepacked   int64
	BudgetExhausted bool
}

// BlobStore is the production verified attachment read path plus daemon-cache
// reader retirement.
type BlobStore interface {
	Open(hash string) (io.ReadSeekCloser, int64, error)
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

type sourceEntry struct {
	oldPackID string
	entry     store.PackIndexEntry
}

// Run reclaims every selected old pack without changing a live mapping until
// every replacement pack has been durably sealed.
func Run(
	ctx context.Context,
	st *store.Store,
	blobs BlobStore,
	attachmentsDir string,
	opts Options,
) (Stats, error) {
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

	pruned, err := st.PruneUnreferencedPackIndex()
	if err != nil {
		return stats, fmt.Errorf("prune unreferenced pack index before repack: %w", err)
	}
	if pruned > int64(math.MaxInt) {
		return stats, fmt.Errorf("pruned mapping count %d exceeds platform int", pruned)
	}
	stats.MappingsPruned = int(pruned)
	if err := ctx.Err(); err != nil {
		return stats, err
	}

	usage, err := st.ListPackUsage()
	if err != nil {
		return stats, err
	}
	selected, exhausted := selectPacks(usage, opts)
	stats.PacksSelected = len(selected)
	stats.BudgetExhausted = exhausted
	if len(selected) == 0 {
		return stats, nil
	}

	var (
		partialIDs  []string
		liveEntries []sourceEntry
	)
	for _, selectedPack := range selected {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if selectedPack.LiveEntries == 0 {
			continue
		}
		entries, err := st.ListReferencedPackEntries(selectedPack.PackID)
		if err != nil {
			return stats, err
		}
		var stored, raw int64
		for _, entry := range entries {
			stored += entry.StoredLen
			raw += entry.RawLen
			liveEntries = append(liveEntries, sourceEntry{
				oldPackID: selectedPack.PackID,
				entry:     entry,
			})
		}
		if int64(len(entries)) != selectedPack.LiveEntries ||
			stored != selectedPack.LiveStoredBytes || raw != selectedPack.LiveRawBytes {
			return stats, fmt.Errorf(
				"pack %s referenced entries changed during repack selection: usage=(%d,%d,%d) entries=(%d,%d,%d)",
				selectedPack.PackID, selectedPack.LiveEntries,
				selectedPack.LiveStoredBytes, selectedPack.LiveRawBytes,
				len(entries), stored, raw)
		}
		partialIDs = append(partialIDs, selectedPack.PackID)
	}

	records, moves, rewriteErr := rewriteLiveEntries(
		ctx, blobs, packsDir, opts.TargetSize, liveEntries, &stats,
	)
	if rewriteErr != nil {
		return stats, rewriteErr
	}
	if len(partialIDs) > 0 {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if err := st.CommitRepack(ctx, partialIDs, records, moves); err != nil {
			return stats, fmt.Errorf("commit repacked attachment mappings: %w", err)
		}
		stats.PacksRewritten = len(partialIDs)
	}

	var cleanupErr error
	for _, selectedPack := range selected {
		if err := ctx.Err(); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			break
		}
		if err := blobs.RetirePack(selectedPack.PackID); err != nil {
			cleanupErr = errors.Join(cleanupErr,
				fmt.Errorf("retire old attachment pack %s: %w", selectedPack.PackID, err))
			continue
		}
		deleted, err := st.DeleteEmptyPackRecord(selectedPack.PackID)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr,
				fmt.Errorf("delete retired attachment pack record %s: %w", selectedPack.PackID, err))
			continue
		}
		if !deleted {
			cleanupErr = errors.Join(cleanupErr,
				fmt.Errorf("delete retired attachment pack record %s: pack still has referenced mappings or record is missing", selectedPack.PackID))
			continue
		}
		stats.PacksRemoved++
	}
	return stats, cleanupErr
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

	var (
		selected        []store.PackUsage
		selectedPartial bool
		selectedRaw     int64
		exhausted       bool
	)
	for _, candidate := range ordered {
		if candidate.LiveEntries == 0 {
			selected = append(selected, candidate)
			continue
		}
		deadStored := candidate.StoredBytes - candidate.LiveStoredBytes
		belowHalf := candidate.EntryCount > 0 &&
			candidate.LiveEntries <= (candidate.EntryCount-1)/2
		oldEnough := !candidate.CreatedAt.After(now.Add(-minPackAge))
		if !belowHalf || !oldEnough || deadStored < minDeadStored {
			continue
		}
		if opts.MaxBytes > 0 && selectedPartial && selectedRaw >= opts.MaxBytes {
			exhausted = true
			continue
		}
		selected = append(selected, candidate)
		selectedPartial = true
		selectedRaw += candidate.LiveRawBytes
	}
	return selected, exhausted
}

func rewriteLiveEntries(
	ctx context.Context,
	blobs BlobStore,
	packsDir string,
	targetSize int64,
	liveEntries []sourceEntry,
	stats *Stats,
) ([]store.PackRecord, []store.RepackMove, error) {
	if len(liveEntries) == 0 {
		return nil, nil, nil
	}

	var (
		writer         packWriter
		currentSources []sourceEntry
		records        []store.PackRecord
		moves          []store.RepackMove
	)
	abort := func(cause error) error {
		if writer == nil {
			return cause
		}
		return errors.Join(cause, writer.Abort())
	}
	seal := func() error {
		if writer == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return abort(err)
		}
		id := writer.ID()
		path := filepath.Join(packsDir, id[:2], id+blobstore.PackExt)
		entries, err := writer.Seal(path)
		if err != nil {
			return abort(fmt.Errorf("seal replacement attachment pack %s: %w", id, err))
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
			moves = append(moves, store.RepackMove{
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
		records = append(records, rec)
		stats.PacksSealed++
		writer = nil
		currentSources = nil
		return nil
	}

	for _, source := range liveEntries {
		if err := ctx.Err(); err != nil {
			return records, moves, abort(err)
		}
		reader, reportedSize, err := blobs.Open(source.entry.BlobHash)
		if err != nil {
			return records, moves, abort(fmt.Errorf(
				"read live blob %s from source pack %s: %w",
				source.entry.BlobHash, source.oldPackID, err))
		}
		data, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			var wrappedReadErr error
			if readErr != nil {
				wrappedReadErr = fmt.Errorf("read live blob %s from source pack %s: %w",
					source.entry.BlobHash, source.oldPackID, readErr)
			}
			return records, moves, abort(errors.Join(
				wrappedReadErr, wrapCloseError(source.entry.BlobHash, closeErr)))
		}
		if reportedSize != int64(len(data)) || source.entry.RawLen != int64(len(data)) {
			return records, moves, abort(fmt.Errorf(
				"live blob %s length mismatch: index=%d reader=%d actual=%d",
				source.entry.BlobHash, source.entry.RawLen, reportedSize, len(data)))
		}

		if writer == nil {
			writer, err = newPackWriter(packsDir, pack.WriterOptions{
				TargetSize: targetSize, ZstdLevel: pack.DefaultZstdLevel,
			})
			if err != nil {
				return records, moves, fmt.Errorf("create replacement attachment pack: %w", err)
			}
		}
		entry, err := writer.Append(data)
		if err != nil {
			return records, moves, abort(fmt.Errorf("append live blob %s to replacement pack: %w", source.entry.BlobHash, err))
		}
		if entry.ID.String() != source.entry.BlobHash {
			return records, moves, abort(fmt.Errorf(
				"appended blob id %s does not match expected %s",
				entry.ID, source.entry.BlobHash))
		}
		currentSources = append(currentSources, source)
		stats.BlobsRepacked++
		stats.BytesRepacked += int64(len(data))
		if writer.Full() {
			if err := seal(); err != nil {
				return records, moves, err
			}
		}
	}
	if err := seal(); err != nil {
		return records, moves, err
	}
	return records, moves, nil
}

func wrapCloseError(hash string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close live blob %s: %w", hash, err)
}
