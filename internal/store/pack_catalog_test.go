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

	stats, err := maintainer.Pack(t.Context(), packstore.PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.True(stats.LooseOrphanSweepSuppressed)
	assert.NoFileExists(filepath.Join(root, validHash[:2], validHash))
	assert.FileExists(filepath.Join(root, orphanHash[:2], orphanHash))
	entry, err := st.GetAttachmentPackEntry(t.Context(), validHash)
	require.NoError(err)
	assert.NotNil(entry)
}

func TestMaintenancePackCatalogResolvesUnknownThumbnailSize(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)
	root := t.TempDir()
	contentHash := pack.ComputeBlobID([]byte("content")).String()
	thumbnail := []byte("thumbnail with no size column")
	thumbnailHash := pack.ComputeBlobID(thumbnail).String()
	thumbnailPath := thumbnailHash[:2] + "/" + thumbnailHash

	fx.addAttachment(contentHash, contentHash[:2]+"/"+contentHash, len("content"))
	fx.setThumbnail(contentHash, thumbnailHash, thumbnailPath)
	require.NoError(os.MkdirAll(filepath.Join(root, thumbnailHash[:2]), 0o700))
	require.NoError(os.WriteFile(filepath.Join(root, filepath.FromSlash(thumbnailPath)), thumbnail, 0o600))

	candidates, err := store.NewMaintenancePackCatalog(st, root).ListUnpacked(t.Context())
	require.NoError(err)
	var got *packstore.Candidate
	for i := range candidates {
		if candidates[i].Hash.String() == thumbnailHash {
			got = &candidates[i]
			break
		}
	}
	require.NotNil(got)
	assert.Equal(t, int64(len(thumbnail)), got.Size)
}

func TestMaintenancePacksThumbnailFromRecordedNoncanonicalPath(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)
	root := t.TempDir()
	contentHash := pack.ComputeBlobID([]byte("remote content")).String()
	thumbnail := []byte("legacy noncanonical thumbnail")
	thumbnailHash := pack.ComputeBlobID(thumbnail).String()
	thumbnailPath := "legacy/thumbnails/" + thumbnailHash

	fx.addAttachment(contentHash, "https://cdn.example.com/content", len("remote content"))
	fx.setThumbnail(contentHash, thumbnailHash, thumbnailPath)
	absThumbnailPath := filepath.Join(root, filepath.FromSlash(thumbnailPath))
	require.NoError(os.MkdirAll(filepath.Dir(absThumbnailPath), 0o700))
	require.NoError(os.WriteFile(absThumbnailPath, thumbnail, 0o600))

	layout, err := packstore.NewLayout(root, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(err)
	maintainer, err := packstore.NewMaintainer(
		store.NewMaintenancePackCatalog(st, root), layout, packstore.MaintainerOptions{},
	)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(maintainer.Close()) })

	stats, err := maintainer.Pack(t.Context(), packstore.PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.NoFileExists(absThumbnailPath)
	entry, err := st.GetAttachmentPackEntry(t.Context(), thumbnailHash)
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
		_, err := h.store.DB().ExecContext(h.t.Context(), h.store.Rebind(`
			DELETE FROM attachments
			WHERE LOWER(content_hash) = ? OR LOWER(thumbnail_hash) = ?`), hash.String(), hash.String())
		require.NoError(h.t, err)
	}
	h.members[hash] = member
}

func (h *msgvaultPackHarness) SetCandidate(candidate packstore.Candidate) {
	h.t.Helper()
	require.NotEmpty(h.t, candidate.Paths)
	_, err := h.store.DB().ExecContext(h.t.Context(), h.store.Rebind(`
		UPDATE attachments SET storage_path = ?, size = ?
		WHERE LOWER(content_hash) = ?`), candidate.Paths[0], candidate.Size, candidate.Hash.String())
	require.NoError(h.t, err)
}

func (h *msgvaultPackHarness) PutPack(record packstore.PackRecord, entries []packstore.IndexEntry) {
	h.t.Helper()
	require.NoError(h.t, h.store.RecordPackedBlobs(h.t.Context(), toStoreRecordTest(record), toStoreEntriesTest(entries)))
}

func (h *msgvaultPackHarness) Snapshot() packstoretest.CatalogState {
	h.t.Helper()
	state := packstoretest.CatalogState{
		Members: make(map[packstore.Hash]bool), Entries: make(map[packstore.Hash]packstore.IndexEntry),
		Packs: make(map[string]packstore.PackRecord),
	}
	references, err := h.store.ListReferencedBlobHashesContext(h.t.Context())
	require.NoError(h.t, err)
	for raw := range references {
		hash, parseErr := packstore.ParseHash(raw)
		require.NoError(h.t, parseErr)
		state.Members[hash] = true
	}
	entries, err := h.store.ListIndexedBlobEntriesContext(h.t.Context())
	require.NoError(h.t, err)
	for _, entry := range entries {
		converted, convertErr := fromStoreEntryTest(entry)
		require.NoError(h.t, convertErr)
		state.Entries[converted.Hash] = converted
	}
	records, err := h.store.ListPackRecordsContext(h.t.Context())
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

func TestPackCatalogCancellationWhileWaitingForDatabase(t *testing.T) {
	// Catalog discovery must pass cancellation into SQL, not only check it
	// before starting. A held connection models contention without a large fixture.
	for _, method := range []string{
		"references", "unpacked", "indexed", "packs", "entries", "resolve", "has-record",
		"record", "adopt", "delete-record", "delete-entry", "clear-metadata",
	} {
		t.Run(method, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			hash, err := packstore.ParseHash(pack.ComputeBlobID([]byte("catalog cancellation")).String())
			require.NoError(err)
			st.DB().SetMaxOpenConns(1)
			conn, err := st.DB().Conn(t.Context())
			require.NoError(err)
			defer func() { _ = conn.Close() }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			initial := st.DB().Stats().WaitCount
			done := make(chan error, 1)
			catalog := store.NewPackCatalog(st)
			go func() {
				var err error
				switch method {
				case "references":
					_, err = catalog.ListReferences(ctx)
				case "unpacked":
					_, err = catalog.ListUnpacked(ctx)
				case "indexed":
					_, err = catalog.ListIndexed(ctx)
				case "packs":
					_, err = catalog.ListPackRecords(ctx)
				case "entries":
					_, err = catalog.ListPackEntries(ctx, "synthetic-pack")
				case "resolve":
					_, err = catalog.Resolve(ctx, hash)
				case "has-record":
					_, err = catalog.HasPackRecord(ctx, pack.NewPackID())
				case "record":
					err = catalog.RecordPack(ctx, packstore.PackRecord{
						PackID: pack.NewPackID(), CreatedAt: time.Now(),
					}, nil)
				case "adopt":
					err = catalog.AdoptPack(ctx, packstore.PackRecord{
						PackID: pack.NewPackID(), CreatedAt: time.Now(),
					}, nil)
				case "delete-record":
					err = catalog.DeletePackRecord(ctx, pack.NewPackID())
				case "delete-entry":
					err = catalog.DeleteIndexEntry(ctx, hash)
				case "clear-metadata":
					err = catalog.ClearPackMetadata(ctx)
				}
				done <- err
			}()
			require.Eventually(func() bool { return st.DB().Stats().WaitCount > initial }, 5*time.Second, 10*time.Millisecond)
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(err, context.Canceled)
			case <-time.After(5 * time.Second):
				require.NoError(conn.Close())
				<-done
				require.FailNow("catalog discovery ignored cancellation while waiting for SQL")
			}
		})
	}
}

func TestPackVerificationWindowListsRowsAsKitReadsThem(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	hash := "aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee"
	oldPack, newPack := pack.NewPackID(), pack.NewPackID()
	created := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	require.NoError(st.RecordPackedBlobs(t.Context(), store.PackRecord{PackID: oldPack, EntryCount: 1, StoredBytes: 64, CreatedAt: created},
		[]store.PackIndexEntry{{BlobHash: hash, PackID: oldPack, Offset: 6, StoredLen: 64, RawLen: 64}}))
	ctx, pass, err := st.BeginPackVerification(t.Context(), 128, 32<<20)
	require.NoError(err)
	// A repack that commits before Pack takes the maintenance lease moves the
	// row; repair must see the new location, or it deletes a live mapping.
	require.NoError(st.AdoptPackedBlobs(store.PackRecord{PackID: newPack, EntryCount: 1, StoredBytes: 64, CreatedAt: created},
		[]store.PackIndexEntry{{BlobHash: hash, PackID: newPack, Offset: 6, StoredLen: 64, RawLen: 64}}))
	// The missing physical pack makes this mapping a verification candidate.
	indexed, err := store.NewMaintenancePackCatalog(st, t.TempDir()).ListIndexed(ctx)
	require.NoError(err)
	require.Len(indexed, 1)
	assert.Equal(newPack, indexed[0].PackID)
	more, err := st.FinishPackVerification(t.Context(), pass)
	require.NoError(err)
	assert.False(more)
}

func TestPackVerificationWindowKeepsItsRangeAcrossListings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	created := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	record := func(hash string) {
		packID := pack.NewPackID()
		require.NoError(st.RecordPackedBlobs(t.Context(), store.PackRecord{PackID: packID, EntryCount: 1, StoredBytes: 64, CreatedAt: created},
			[]store.PackIndexEntry{{BlobHash: hash, PackID: packID, Offset: 6, StoredLen: 64, RawLen: 64}}))
	}
	inWindow := "aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee"
	record(inWindow)
	ctx, pass, err := st.BeginPackVerification(t.Context(), 128, 32<<20)
	require.NoError(err)
	// Missing physical packs make these mappings verification candidates.
	catalog := store.NewMaintenancePackCatalog(st, t.TempDir())
	repair, err := catalog.ListIndexed(ctx)
	require.NoError(err)
	require.Len(repair, 1)
	// Pack can add a row and move a selected mapping between listings.
	record("bb11223344556677889900aabbccddeeff00112233445566778899aabbccddee")
	movedPack := pack.NewPackID()
	require.NoError(st.AdoptPackedBlobs(store.PackRecord{PackID: movedPack, EntryCount: 1, StoredBytes: 64, CreatedAt: created},
		[]store.PackIndexEntry{{BlobHash: inWindow, PackID: movedPack, Offset: 6, StoredLen: 64, RawLen: 64}}))
	sweep, err := catalog.ListIndexed(ctx)
	require.NoError(err)
	require.Len(sweep, 1, "the sweep covers the window repair verified, not rows added after it")
	assert.Equal(inWindow, sweep[0].Hash.String())
	assert.Equal(movedPack, sweep[0].PackID, "the selected mapping is reread")
	more, err := st.FinishPackVerification(t.Context(), pass)
	require.NoError(err)
	assert.False(more)
}

// TestSkipLooseEnumerationReportsNoCandidates checks that a verification
// follow-up skips loose candidates.
func TestSkipLooseEnumerationReportsNoCandidates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)
	root := t.TempDir()
	content := []byte("loose blob awaiting a pass that enumerates candidates")
	hash := pack.ComputeBlobID(content).String()
	loosePath := filepath.Join(root, hash[:2], hash)
	require.NoError(os.MkdirAll(filepath.Dir(loosePath), 0o700))
	require.NoError(os.WriteFile(loosePath, content, 0o600))
	fx.addAttachment(hash, hash[:2]+"/"+hash, len(content))

	catalog := store.NewMaintenancePackCatalog(st, root)
	candidates, err := catalog.ListUnpacked(t.Context())
	require.NoError(err)
	require.Len(candidates, 1, "the loose blob is a candidate without the flag")

	ctx, pass, err := st.BeginPackVerification(t.Context(), 128, 32<<20)
	require.NoError(err)
	pass.SkipLooseEnumeration()
	skipped, err := catalog.ListUnpacked(ctx)
	require.NoError(err)
	assert.Empty(skipped, "a suppressed pass reports no loose candidates")
}

func TestPackVerificationSelectsLooseAndMissingPackMappings(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	layout, err := packstore.NewLayout(root, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(err)
	created := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	present, missing, unrecorded := pack.NewPackID(), pack.NewPackID(), pack.NewPackID()
	healthy := fmt.Sprintf("%064x", 1)
	loose := fmt.Sprintf("%064x", 2)
	absent := fmt.Sprintf("%064x", 3)
	dangling := fmt.Sprintf("%064x", 4)
	record := func(id string, hashes ...string) {
		entries := make([]store.PackIndexEntry, 0, len(hashes))
		for _, hash := range hashes {
			entries = append(entries, store.PackIndexEntry{BlobHash: hash, PackID: id, Offset: 6, StoredLen: 1, RawLen: 1})
		}
		require.NoError(st.RecordPackedBlobs(t.Context(), store.PackRecord{PackID: id, EntryCount: int64(len(entries)), StoredBytes: int64(len(entries)), CreatedAt: created}, entries))
	}
	record(present, healthy, loose)
	record(missing, absent)
	record(unrecorded, dangling)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM attachment_packs WHERE pack_id = ?`), unrecorded)
	require.NoError(err)
	require.NoError(os.MkdirAll(filepath.Dir(layout.PackPath(present)), 0o700))
	require.NoError(os.WriteFile(layout.PackPath(present), []byte("synthetic"), 0o600))
	hash, err := packstore.ParseHash(loose)
	require.NoError(err)
	require.NoError(os.MkdirAll(filepath.Dir(layout.CompressedLoosePath(hash)), 0o700))
	require.NoError(os.WriteFile(layout.CompressedLoosePath(hash), []byte("synthetic"), 0o600))
	catalog := store.NewMaintenancePackCatalog(st, root)
	ctx, pass, err := st.BeginPackVerification(t.Context(), 2, 100)
	require.NoError(err)
	first, err := catalog.ListIndexed(ctx)
	require.NoError(err)
	require.Len(first, 2)
	assert.Equal([]string{loose, absent}, []string{first[0].Hash.String(), first[1].Hash.String()})
	again, err := catalog.ListIndexed(ctx)
	require.NoError(err)
	assert.Equal(first, again)
	more, err := st.FinishPackVerification(t.Context(), pass)
	require.NoError(err)
	require.True(more)
	ctx, pass, err = st.BeginPackVerification(t.Context(), 2, 100)
	require.NoError(err)
	last, err := catalog.ListIndexed(ctx)
	require.NoError(err)
	require.Len(last, 1)
	assert.Equal(dangling, last[0].Hash.String())
	more, err = st.FinishPackVerification(t.Context(), pass)
	require.NoError(err)
	assert.False(more)
}

func TestPackVerificationCanceledCheckpointReplaysWindow(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	id := pack.NewPackID()
	hash := fmt.Sprintf("%064x", 1)
	require.NoError(t, st.RecordPackedBlobs(t.Context(), store.PackRecord{PackID: id, EntryCount: 1, StoredBytes: 1, CreatedAt: time.Now()},
		[]store.PackIndexEntry{{BlobHash: hash, PackID: id, Offset: 6, StoredLen: 1, RawLen: 1}}))
	catalog := store.NewMaintenancePackCatalog(st, root)
	ctx, pass, err := st.BeginPackVerification(t.Context(), 1, 100)
	require.NoError(t, err)
	first, err := catalog.ListIndexed(ctx)
	require.NoError(t, err)
	require.Len(t, first, 1)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.FinishPackVerification(canceled, pass)
	require.Error(t, err)
	ctx, _, err = st.BeginPackVerification(t.Context(), 1, 100)
	require.NoError(t, err)
	replayed, err := catalog.ListIndexed(ctx)
	require.NoError(t, err)
	assert.Equal(t, first, replayed)
}

func TestPackVerificationWindowFindsIndexedHashAfterUnindexedFiles(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	for i := range 300 {
		hash := fmt.Sprintf("%064x", i+1)
		path := filepath.Join(root, hash[:2], hash)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("synthetic"), 0o600))
	}
	id := pack.NewPackID()
	indexed := fmt.Sprintf("%064x", 300)
	require.NoError(t, st.RecordPackedBlobs(t.Context(), store.PackRecord{PackID: id, EntryCount: 1, StoredBytes: 1, CreatedAt: time.Now()},
		[]store.PackIndexEntry{{BlobHash: indexed, PackID: id, Offset: 6, StoredLen: 1, RawLen: 1}}))
	ctx, pass, err := st.BeginPackVerification(t.Context(), 1, 100)
	require.NoError(t, err)
	entries, err := store.NewMaintenancePackCatalog(st, root).ListIndexed(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, indexed, entries[0].Hash.String())
	more, err := st.FinishPackVerification(t.Context(), pass)
	require.NoError(t, err)
	assert.False(t, more)
}
