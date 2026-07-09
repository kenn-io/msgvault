package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRecordAndGetPackedBlobs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	rec := store.PackRecord{
		PackID:      "01hzy3v7q8r9s0t1u2v3w4x5y6",
		EntryCount:  2,
		StoredBytes: 4096,
		CreatedAt:   time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	entries := []store.PackIndexEntry{
		{BlobHash: "aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee",
			PackID: rec.PackID, Offset: 6, StoredLen: 2048, RawLen: 4000, Flags: 1, CRC32C: 4022250974},
		{BlobHash: "bb11223344556677889900aabbccddeeff00112233445566778899aabbccddee",
			PackID: rec.PackID, Offset: 2054, StoredLen: 2048, RawLen: 2048, Flags: 0, CRC32C: 1},
	}
	require.NoError(st.RecordPackedBlobs(rec, entries))

	got, err := st.GetAttachmentPackEntry(entries[0].BlobHash)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(entries[0], *got)

	// CRC32C above int32 max must round-trip on both backends (BIGINT column).
	assert.Equal(uint32(4022250974), got.CRC32C)

	missing, err := st.GetAttachmentPackEntry(
		"cc11223344556677889900aabbccddeeff00112233445566778899aabbccddee")
	require.NoError(err)
	assert.Nil(missing)

	// Idempotent re-record (crash-reconciliation re-runs adoption).
	require.NoError(st.RecordPackedBlobs(rec, entries))
}

func TestRecordPackedBlobsRejectsMismatchedPackID(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	rec := store.PackRecord{
		PackID:      "01hzy3v7q8r9s0t1u2v3w4x5y6",
		EntryCount:  1,
		StoredBytes: 64,
		CreatedAt:   time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	entries := []store.PackIndexEntry{
		{BlobHash: "dd11223344556677889900aabbccddeeff00112233445566778899aabbccddee",
			PackID: "01hzy3v7q8r9s0t1u2v3w4x5y7", Offset: 6, StoredLen: 64, RawLen: 64},
	}
	err := st.RecordPackedBlobs(rec, entries)
	require.Error(err)

	// The mismatch must fail the whole call: no index row was written.
	got, err := st.GetAttachmentPackEntry(entries[0].BlobHash)
	require.NoError(err)
	require.Nil(got)
}

// packTestHash returns a synthetic 64-char blob hash unique per prefix.
func packTestHash(prefix string) string {
	const filler = "0000000000000000000000000000000000000000000000000000000000000000"
	return prefix + filler[len(prefix):]
}

func packTestRecord(packID string, hashes ...string) (store.PackRecord, []store.PackIndexEntry) {
	rec := store.PackRecord{
		PackID:      packID,
		EntryCount:  int64(len(hashes)),
		StoredBytes: int64(len(hashes)) * 128,
		CreatedAt:   time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	entries := make([]store.PackIndexEntry, 0, len(hashes))
	for i, h := range hashes {
		entries = append(entries, store.PackIndexEntry{
			BlobHash: h, PackID: packID,
			Offset: int64(6 + i*128), StoredLen: 128, RawLen: 256,
		})
	}
	return rec, entries
}

// packAttachmentFixture seeds one message and returns a helper that inserts
// attachment rows for it, plus one to set thumbnail columns (no public API
// writes thumbnails yet, so those are set with direct SQL).
type packAttachmentFixture struct {
	t     *testing.T
	store *store.Store
	msgID int64
	seq   int
}

func newPackAttachmentFixture(t *testing.T, st *store.Store) *packAttachmentFixture {
	t.Helper()
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-pack", "Pack Thread")
	require.NoError(t, err, "EnsureConversation")
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "pack-msg",
		MessageType:     "email",
		SizeEstimate:    100,
	})
	require.NoError(t, err, "UpsertMessage")
	return &packAttachmentFixture{t: t, store: st, msgID: msgID}
}

// addAttachment inserts an attachment row with the given content hash, path,
// and size, returning nothing; rows are distinguished by unique filenames.
func (f *packAttachmentFixture) addAttachment(hash, path string, size int) {
	f.t.Helper()
	f.seq++
	name := fmt.Sprintf("file-%d.bin", f.seq)
	err := f.store.UpsertAttachment(f.msgID, name, "application/octet-stream",
		path, hash, size)
	require.NoErrorf(f.t, err, "UpsertAttachment(%s)", name)
}

// setThumbnail sets thumbnail columns on the newest row with contentHash.
func (f *packAttachmentFixture) setThumbnail(contentHash, thumbHash, thumbPath string) {
	f.t.Helper()
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachments SET thumbnail_hash = ?, thumbnail_path = ?
		WHERE id = (SELECT MAX(id) FROM attachments WHERE content_hash = ?)`),
		thumbHash, thumbPath, contentHash)
	require.NoErrorf(f.t, err, "setThumbnail(%s)", thumbHash)
}

func (f *packAttachmentFixture) pathsForContentHash(hash string) []string {
	f.t.Helper()
	rows, err := f.store.DB().Query(f.store.Rebind(`
		SELECT storage_path FROM attachments WHERE content_hash = ? ORDER BY id`), hash)
	require.NoError(f.t, err, "query storage_path")
	defer rows.Close() //nolint:errcheck // read-only cursor
	var paths []string
	for rows.Next() {
		var p string
		require.NoError(f.t, rows.Scan(&p), "scan storage_path")
		paths = append(paths, p)
	}
	require.NoError(f.t, rows.Err(), "rows.Err")
	return paths
}

func (f *packAttachmentFixture) thumbnailPathForHash(thumbHash string) string {
	f.t.Helper()
	var p string
	err := f.store.DB().QueryRow(f.store.Rebind(`
		SELECT thumbnail_path FROM attachments WHERE thumbnail_hash = ?`), thumbHash).Scan(&p)
	require.NoErrorf(f.t, err, "query thumbnail_path for %s", thumbHash)
	return p
}

func TestRecordPackedBlobsCanonicalizesPaths(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	hashLoose := packTestHash("aa01")
	hashCanonical := packTestHash("bb02")
	hashURL := packTestHash("cc03")
	hashThumb := packTestHash("dd04")
	hashThumbURL := packTestHash("ee05")
	hashUnpacked := packTestHash("ff06")

	canonicalOf := func(h string) string { return h[:2] + "/" + h }

	fx.addAttachment(hashLoose, "synctech-sms/aa/"+hashLoose, 100)
	fx.addAttachment(hashCanonical, canonicalOf(hashCanonical), 100)
	fx.addAttachment(hashURL, "https://cdn.example.com/"+hashURL, 100)
	fx.addAttachment(hashThumb, canonicalOf(hashThumb), 100)
	fx.setThumbnail(hashThumb, hashThumb, "legacy-thumbs/"+hashThumb)
	fx.addAttachment(hashThumbURL, canonicalOf(hashThumbURL), 100)
	fx.setThumbnail(hashThumbURL, hashThumbURL, "http://cdn.example.com/t/"+hashThumbURL)
	fx.addAttachment(hashUnpacked, "synctech-sms/ff/"+hashUnpacked, 100)

	rec, entries := packTestRecord("01hzy3v7q8r9s0t1u2v3w4x5z1",
		hashLoose, hashCanonical, hashURL, hashThumb, hashThumbURL)
	require.NoError(st.RecordPackedBlobs(rec, entries))

	assert.Equal([]string{canonicalOf(hashLoose)}, fx.pathsForContentHash(hashLoose),
		"noncanonical storage_path is rewritten")
	assert.Equal([]string{canonicalOf(hashCanonical)}, fx.pathsForContentHash(hashCanonical),
		"canonical storage_path is preserved")
	assert.Equal([]string{"https://cdn.example.com/" + hashURL}, fx.pathsForContentHash(hashURL),
		"URL storage_path is never rewritten")
	assert.Equal(canonicalOf(hashThumb), fx.thumbnailPathForHash(hashThumb),
		"noncanonical thumbnail_path is rewritten")
	assert.Equal("http://cdn.example.com/t/"+hashThumbURL, fx.thumbnailPathForHash(hashThumbURL),
		"URL thumbnail_path is never rewritten")
	assert.Equal([]string{"synctech-sms/ff/" + hashUnpacked}, fx.pathsForContentHash(hashUnpacked),
		"paths of hashes outside the pack are untouched")

	// Canonicalization and index inserts share one transaction: the index
	// rows for the same call must be visible too.
	got, err := st.GetAttachmentPackEntry(hashLoose)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(rec.PackID, got.PackID)
}

func TestRecordPackedBlobsRejectsMalformedHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	hashGood := packTestHash("aa11")
	fx.addAttachment(hashGood, "synctech-sms/aa/"+hashGood, 100)

	rec, entries := packTestRecord("01hzy3v7q8r9s0t1u2v3w4x5z2", hashGood)
	entries = append(entries, store.PackIndexEntry{
		BlobHash: "deadbeef", PackID: rec.PackID, Offset: 134, StoredLen: 8, RawLen: 8,
	})
	rec.EntryCount = 2

	err := st.RecordPackedBlobs(rec, entries)
	require.Error(err)
	assert.Contains(err.Error(), "malformed blob hash")

	// Nothing was written: no pack record, no index rows, no path rewrite.
	has, err := st.HasPackRecord(rec.PackID)
	require.NoError(err)
	assert.False(has, "pack record must not exist")
	got, err := st.GetAttachmentPackEntry(hashGood)
	require.NoError(err)
	assert.Nil(got, "no index row for the valid entry either")
	assert.Equal([]string{"synctech-sms/aa/" + hashGood}, fx.pathsForContentHash(hashGood),
		"storage_path untouched")
}

func TestListUnpackedBlobs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	hashLoose := packTestHash("aa21")
	hashURL := packTestHash("bb22")
	hashPacked := packTestHash("cc23")
	hashEmpty := packTestHash("dd24")
	hashThumbOnly := packTestHash("ee25")
	hashBoth := packTestHash("ff26")

	fx.addAttachment(hashLoose, hashLoose[:2]+"/"+hashLoose, 100)
	fx.addAttachment(hashURL, "https://cdn.example.com/"+hashURL, 100)
	fx.addAttachment(hashPacked, hashPacked[:2]+"/"+hashPacked, 100)
	fx.addAttachment(hashEmpty, "", 100)
	// hashBoth appears as a content blob AND as another row's thumbnail;
	// it must be listed once, with the content row's size.
	fx.addAttachment(hashBoth, hashBoth[:2]+"/"+hashBoth, 300)
	fx.setThumbnail(hashLoose, hashBoth, "thumbs/"+hashBoth)
	fx.setThumbnail(hashPacked, hashThumbOnly, "thumbs/"+hashThumbOnly)

	rec, entries := packTestRecord("01hzy3v7q8r9s0t1u2v3w4x5z3", hashPacked)
	require.NoError(st.RecordPackedBlobs(rec, entries))

	blobs, err := st.ListUnpackedBlobs()
	require.NoError(err)

	byHash := make(map[string]store.UnpackedBlob, len(blobs))
	for _, b := range blobs {
		_, dup := byHash[b.Hash]
		require.Falsef(dup, "duplicate hash %s in ListUnpackedBlobs", b.Hash)
		byHash[b.Hash] = b
	}

	assert.NotContains(byHash, hashURL, "URL-backed blobs are skipped")
	assert.NotContains(byHash, hashPacked, "already-indexed blobs are skipped")
	assert.NotContains(byHash, hashEmpty, "empty-path rows are skipped")

	require.Contains(byHash, hashLoose)
	assert.Equal(store.UnpackedBlob{
		Hash: hashLoose, Path: hashLoose[:2] + "/" + hashLoose, Size: 100,
	}, byHash[hashLoose])

	require.Contains(byHash, hashBoth)
	assert.Equal(int64(300), byHash[hashBoth].Size,
		"content-vs-thumbnail dup keeps the content row (with size)")
	assert.Equal(hashBoth[:2]+"/"+hashBoth, byHash[hashBoth].Path)

	require.Contains(byHash, hashThumbOnly)
	assert.Equal(store.UnpackedBlob{
		Hash: hashThumbOnly, Path: "thumbs/" + hashThumbOnly, Size: -1,
	}, byHash[hashThumbOnly], "thumbnail-only blobs have Size -1")

	assert.Len(blobs, 3)
}

func TestPackRecordLifecycle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	hashA := packTestHash("aa31")
	hashB := packTestHash("bb32")
	hashC := packTestHash("cc33")

	recA, entriesA := packTestRecord("01hzy3v7q8r9s0t1u2v3w4x5z4", hashA, hashB)
	recB, entriesB := packTestRecord("01hzy3v7q8r9s0t1u2v3w4x5z5", hashC)
	require.NoError(st.RecordPackedBlobs(recA, entriesA))
	require.NoError(st.RecordPackedBlobs(recB, entriesB))

	recs, err := st.ListPackRecords()
	require.NoError(err)
	assert.Equal([]store.PackRecord{recA, recB}, recs, "ordered by pack_id")

	hashes, err := st.ListIndexedBlobHashes()
	require.NoError(err)
	assert.Equal(map[string]struct{}{
		hashA: {}, hashB: {}, hashC: {},
	}, hashes)

	has, err := st.HasPackRecord(recA.PackID)
	require.NoError(err)
	assert.True(has)
	has, err = st.HasPackRecord("01hzy3v7q8r9s0t1u2v3w4x5z9")
	require.NoError(err)
	assert.False(has)

	n, err := st.CountPackIndexEntries(recA.PackID)
	require.NoError(err)
	assert.Equal(int64(2), n)
	n, err = st.CountPackIndexEntries("01hzy3v7q8r9s0t1u2v3w4x5z9")
	require.NoError(err)
	assert.Zero(n)

	require.NoError(st.DeletePackRecord(recA.PackID))

	has, err = st.HasPackRecord(recA.PackID)
	require.NoError(err)
	assert.False(has, "attachment_packs row removed")
	n, err = st.CountPackIndexEntries(recA.PackID)
	require.NoError(err)
	assert.Zero(n, "index rows removed")
	got, err := st.GetAttachmentPackEntry(hashA)
	require.NoError(err)
	assert.Nil(got)

	// The other pack is untouched.
	recs, err = st.ListPackRecords()
	require.NoError(err)
	assert.Equal([]store.PackRecord{recB}, recs)

	// Deleting an absent pack is not an error (idempotent cleanup).
	require.NoError(st.DeletePackRecord(recA.PackID))
}
