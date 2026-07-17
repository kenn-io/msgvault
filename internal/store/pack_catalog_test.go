package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func TestPackCatalogMalformedReferenceDoesNotBlockValidPackingOrPermitOrphanSweep(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)
	root := t.TempDir()
	writeLoose := func(content []byte) string {
		hash := pack.ComputeBlobID(content).String()
		path := filepath.Join(root, hash[:2], hash)
		require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(os.WriteFile(path, content, 0o600))
		return hash
	}
	validHash := writeLoose([]byte("valid content must still be packed"))
	fx.addAttachment(validHash, validHash[:2]+"/"+validHash, 34)
	fx.addAttachmentOnNewMessage("BAD-HASH", "malformed/BAD-HASH", 8)
	orphanHash := writeLoose([]byte("incomplete reachability must preserve this loose object"))
	layout, err := packstore.NewLayout(root, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(err)
	maintainer, err := packstore.NewMaintainer(store.NewPackCatalog(st), layout, packstore.MaintainerOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(maintainer.Close()) })

	stats, err := maintainer.Pack(context.Background(), packstore.PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.True(stats.LooseOrphanSweepSuppressed)
	assert.NoFileExists(filepath.Join(root, validHash[:2], validHash))
	assert.FileExists(filepath.Join(root, orphanHash[:2], orphanHash))
	entry, err := st.GetAttachmentPackEntry(validHash)
	require.NoError(err)
	assert.NotNil(entry)
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
