package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/packstore/packstoretest"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPackCatalogContract(t *testing.T) {
	packstoretest.RunCatalogContract(t, newMsgvaultPackHarness, packstoretest.ContractOptions{
		Now:       time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		NewPackID: pack.NewPackID,
	})
}

type msgvaultPackHarness struct {
	t       *testing.T
	store   *store.Store
	fixture *packAttachmentFixture
	members map[packstore.Hash]bool
}

func newMsgvaultPackHarness(t *testing.T) packstoretest.CatalogHarness {
	t.Helper()
	st := testutil.NewTestStore(t)
	return &msgvaultPackHarness{t: t, store: st, fixture: newPackAttachmentFixture(t, st),
		members: make(map[packstore.Hash]bool)}
}

func (h *msgvaultPackHarness) Catalog() packstore.Catalog {
	return store.NewPackCatalog(h.store)
}

func (h *msgvaultPackHarness) SetMember(hash packstore.Hash, member bool) {
	h.t.Helper()
	if member && !h.members[hash] {
		h.fixture.addAttachment(hash.String(), hash.String()[:2]+"/"+hash.String(), 13)
	}
	if !member && h.members[hash] {
		_, err := h.store.DB().Exec(h.store.Rebind(`
			DELETE FROM attachments
			WHERE LOWER(content_hash) = ? OR LOWER(thumbnail_hash) = ?`), hash.String(), hash.String())
		require.NoError(h.t, err)
	}
	h.members[hash] = member
}

func (h *msgvaultPackHarness) SetCandidate(candidate packstore.Candidate) {
	h.t.Helper()
	require.NotEmpty(h.t, candidate.Paths)
	_, err := h.store.DB().Exec(h.store.Rebind(`
		UPDATE attachments SET storage_path = ?, size = ?
		WHERE LOWER(content_hash) = ?`), candidate.Paths[0], candidate.Size, candidate.Hash.String())
	require.NoError(h.t, err)
}

func (h *msgvaultPackHarness) PutPack(record packstore.PackRecord, entries []packstore.IndexEntry) {
	h.t.Helper()
	require.NoError(h.t, h.store.RecordPackedBlobs(toStoreRecordTest(record), toStoreEntriesTest(entries)))
}

func (h *msgvaultPackHarness) Snapshot() packstoretest.CatalogState {
	h.t.Helper()
	state := packstoretest.CatalogState{
		Members: make(map[packstore.Hash]bool), Entries: make(map[packstore.Hash]packstore.IndexEntry),
		Packs: make(map[string]packstore.PackRecord),
	}
	references, err := h.store.ListReferencedBlobHashes()
	require.NoError(h.t, err)
	for raw := range references {
		hash, parseErr := packstore.ParseHash(raw)
		require.NoError(h.t, parseErr)
		state.Members[hash] = true
	}
	entries, err := h.store.ListIndexedBlobEntries()
	require.NoError(h.t, err)
	for _, entry := range entries {
		converted, convertErr := fromStoreEntryTest(entry)
		require.NoError(h.t, convertErr)
		state.Entries[converted.Hash] = converted
	}
	records, err := h.store.ListPackRecords()
	require.NoError(h.t, err)
	for _, record := range records {
		converted := fromStoreRecordTest(record)
		state.Packs[converted.PackID] = converted
	}
	return state
}

func toStoreRecordTest(record packstore.PackRecord) store.PackRecord {
	return store.PackRecord{PackID: record.PackID, EntryCount: record.EntryCount,
		StoredBytes: record.StoredBytes, CreatedAt: record.CreatedAt}
}

func fromStoreRecordTest(record store.PackRecord) packstore.PackRecord {
	return packstore.PackRecord{PackID: record.PackID, EntryCount: record.EntryCount,
		StoredBytes: record.StoredBytes, CreatedAt: record.CreatedAt}
}

func toStoreEntriesTest(entries []packstore.IndexEntry) []store.PackIndexEntry {
	result := make([]store.PackIndexEntry, len(entries))
	for i, entry := range entries {
		result[i] = store.PackIndexEntry{BlobHash: entry.Hash.String(), PackID: entry.PackID,
			Offset: entry.Offset, StoredLen: entry.StoredLen, RawLen: entry.RawLen,
			Flags: entry.Flags, CRC32C: entry.CRC32C}
	}
	return result
}

func fromStoreEntryTest(entry store.PackIndexEntry) (packstore.IndexEntry, error) {
	hash, err := packstore.ParseHash(entry.BlobHash)
	if err != nil {
		return packstore.IndexEntry{}, fmt.Errorf("parse test pack index hash %q: %w", entry.BlobHash, err)
	}
	return packstore.IndexEntry{Hash: hash, PackID: entry.PackID, Offset: entry.Offset,
		StoredLen: entry.StoredLen, RawLen: entry.RawLen, Flags: entry.Flags, CRC32C: entry.CRC32C}, nil
}
