package store

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"go.kenn.io/kit/packstore"
)

// PackCatalog adapts msgvault's attachment schema to Kit's physical packed-CAS
// engine. It contains conversions only; SQL and transaction authority remain
// on Store.
type PackCatalog struct {
	store *Store
}

// NewPackCatalog constructs a Kit catalog adapter over st.
func NewPackCatalog(st *Store) *PackCatalog { return &PackCatalog{store: st} }

var _ packstore.Catalog = (*PackCatalog)(nil)

func (c *PackCatalog) Resolve(ctx context.Context, hash packstore.Hash) (packstore.Location, error) {
	if err := ctx.Err(); err != nil {
		return packstore.Location{}, err
	}
	location, err := c.store.ResolveAttachmentBlob(hash.String())
	if err != nil {
		return packstore.Location{}, err
	}
	result := packstore.Location{Member: location.Referenced}
	if location.Pack != nil {
		entry, err := fromStorePackEntry(*location.Pack)
		if err != nil {
			return packstore.Location{}, err
		}
		result.Pack = &entry
	}
	return result, nil
}

func (c *PackCatalog) ListReferences(ctx context.Context) (packstore.ReferenceInventory, error) {
	if err := ctx.Err(); err != nil {
		return packstore.ReferenceInventory{}, err
	}
	raw, err := c.store.ListReferencedBlobHashes()
	if err != nil {
		return packstore.ReferenceInventory{}, err
	}
	byHash := make(map[packstore.Hash][]string)
	complete := true
	for original := range raw {
		hash, err := packstore.ParseHash(strings.ToLower(original))
		if err != nil {
			complete = false
			slog.Error("malformed referenced attachment hash; suppressing loose orphan deletion",
				"original_hash", original, "error", err)
			continue
		}
		byHash[hash] = append(byHash[hash], original)
	}
	result := make([]packstore.Reference, 0, len(byHash))
	for hash, aliases := range byHash {
		sort.Strings(aliases)
		result = append(result, packstore.Reference{Hash: hash, OriginalHashes: aliases})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Hash < result[j].Hash })
	return packstore.ReferenceInventory{References: result, Complete: complete}, nil
}

func (c *PackCatalog) ListUnpacked(ctx context.Context) ([]packstore.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := c.store.ListUnpackedBlobs()
	if err != nil {
		return nil, err
	}
	result := make([]packstore.Candidate, 0, len(raw))
	for _, blob := range raw {
		hash, err := packstore.ParseHash(strings.ToLower(blob.Hash))
		if err != nil {
			slog.Error("malformed unpacked attachment hash; preserving recorded candidates",
				"original_hash", blob.Hash, "error", err)
			continue
		}
		result = append(result, packstore.Candidate{Hash: hash,
			OriginalHashes: blob.OriginalHashes, Paths: blob.Paths, Size: blob.Size})
	}
	return result, nil
}

func (c *PackCatalog) ListIndexed(ctx context.Context) ([]packstore.IndexEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := c.store.ListIndexedBlobEntries()
	if err != nil {
		return nil, err
	}
	result := make([]packstore.IndexEntry, 0, len(raw))
	for _, entry := range raw {
		converted, err := fromStorePackEntry(entry)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Hash < result[j].Hash })
	return result, nil
}

func (c *PackCatalog) ListPackRecords(ctx context.Context) ([]packstore.PackRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := c.store.ListPackRecords()
	if err != nil {
		return nil, err
	}
	result := make([]packstore.PackRecord, len(raw))
	for i, record := range raw {
		result[i] = fromStorePackRecord(record)
	}
	return result, nil
}

func (c *PackCatalog) ListPackEntries(ctx context.Context, packID string) ([]packstore.IndexEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := c.store.ListAttachmentPackEntries(packID)
	if err != nil {
		return nil, err
	}
	return fromStorePackEntries(raw)
}

func (c *PackCatalog) HasPackRecord(ctx context.Context, packID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return c.store.HasPackRecord(packID)
}

func (c *PackCatalog) PruneUnreferenced(ctx context.Context) (int64, error) {
	return c.store.PruneUnreferencedPackIndex(ctx)
}

func (c *PackCatalog) RecordPack(ctx context.Context, record packstore.PackRecord, adoptions []packstore.Adoption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.store.RecordPackedBlobsWithAliases(toStorePackRecord(record), toStoreAdoptions(adoptions))
}

func (c *PackCatalog) AdoptPack(ctx context.Context, record packstore.PackRecord, adoptions []packstore.Adoption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.store.AdoptPackedBlobsWithAliases(toStorePackRecord(record), toStoreAdoptions(adoptions))
}

func (c *PackCatalog) DeletePackRecord(ctx context.Context, packID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.store.DeletePackRecord(packID)
}

func (c *PackCatalog) DeleteIndexEntry(ctx context.Context, hash packstore.Hash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.store.DeletePackIndexEntry(hash.String())
}

func (c *PackCatalog) ListPackUsage(ctx context.Context) ([]packstore.PackUsage, error) {
	raw, err := c.store.ListPackUsage(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]packstore.PackUsage, len(raw))
	for i, usage := range raw {
		result[i] = packstore.PackUsage{PackRecord: fromStorePackRecord(usage.PackRecord),
			LiveEntries: usage.LiveEntries, LiveStoredBytes: usage.LiveStoredBytes,
			LiveRawBytes: usage.LiveRawBytes, MaxLiveStoredLen: usage.MaxLiveStoredLen,
			MaxLiveRawLen: usage.MaxLiveRawLen}
	}
	return result, nil
}

func (c *PackCatalog) ListLivePackEntries(ctx context.Context, packID string) ([]packstore.IndexEntry, error) {
	raw, err := c.store.ListReferencedPackEntries(ctx, packID)
	if err != nil {
		return nil, err
	}
	return fromStorePackEntries(raw)
}

func (c *PackCatalog) CommitRepack(ctx context.Context, sourceIDs []string, records []packstore.PackRecord, moves []packstore.RepackMove) error {
	storeRecords := make([]PackRecord, len(records))
	for i, record := range records {
		storeRecords[i] = toStorePackRecord(record)
	}
	storeMoves := make([]RepackMove, len(moves))
	for i, move := range moves {
		storeMoves[i] = RepackMove{OldPackID: move.OldPackID, NewEntry: toStorePackEntry(move.NewEntry)}
	}
	return c.store.CommitRepack(ctx, sourceIDs, storeRecords, storeMoves)
}

func (c *PackCatalog) DeleteEmptyPackRecord(ctx context.Context, packID string) (bool, error) {
	return c.store.DeleteEmptyPackRecord(ctx, packID)
}

func (c *PackCatalog) ClearPackMetadata(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.store.ClearAttachmentPackMetadata()
}

func fromStorePackEntry(entry PackIndexEntry) (packstore.IndexEntry, error) {
	hash, err := packstore.ParseHash(entry.BlobHash)
	if err != nil {
		return packstore.IndexEntry{}, fmt.Errorf("parse pack index hash %q: %w", entry.BlobHash, err)
	}
	result := packstore.IndexEntry{Hash: hash, PackID: entry.PackID, Offset: entry.Offset,
		StoredLen: entry.StoredLen, RawLen: entry.RawLen, Flags: entry.Flags, CRC32C: entry.CRC32C}
	if err := result.Validate(); err != nil {
		return packstore.IndexEntry{}, fmt.Errorf("validate converted pack index entry %s: %w", entry.BlobHash, err)
	}
	return result, nil
}

func fromStorePackEntries(entries []PackIndexEntry) ([]packstore.IndexEntry, error) {
	result := make([]packstore.IndexEntry, len(entries))
	for i, entry := range entries {
		converted, err := fromStorePackEntry(entry)
		if err != nil {
			return nil, err
		}
		result[i] = converted
	}
	return result, nil
}

func toStorePackEntry(entry packstore.IndexEntry) PackIndexEntry {
	return PackIndexEntry{BlobHash: entry.Hash.String(), PackID: entry.PackID,
		Offset: entry.Offset, StoredLen: entry.StoredLen, RawLen: entry.RawLen,
		Flags: entry.Flags, CRC32C: entry.CRC32C}
}

func toStoreAdoptions(adoptions []packstore.Adoption) []PackIndexAdoption {
	result := make([]PackIndexAdoption, len(adoptions))
	for i, adoption := range adoptions {
		result[i] = PackIndexAdoption{Entry: toStorePackEntry(adoption.Entry),
			OriginalHashes: adoption.OriginalHashes}
	}
	return result
}

func fromStorePackRecord(record PackRecord) packstore.PackRecord {
	return packstore.PackRecord{PackID: record.PackID, EntryCount: record.EntryCount,
		StoredBytes: record.StoredBytes, CreatedAt: record.CreatedAt}
}

func toStorePackRecord(record packstore.PackRecord) PackRecord {
	return PackRecord{PackID: record.PackID, EntryCount: record.EntryCount,
		StoredBytes: record.StoredBytes, CreatedAt: record.CreatedAt}
}
