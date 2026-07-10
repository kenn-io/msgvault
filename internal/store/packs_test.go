package store_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRecordAndGetPackedBlobs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	rec := store.PackRecord{
		PackID:      "01hzy3v7q8r9s0t1a2v3w4x5y6",
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
		PackID:      "01hzy3v7q8r9s0t1a2v3w4x5y6",
		EntryCount:  1,
		StoredBytes: 64,
		CreatedAt:   time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	entries := []store.PackIndexEntry{
		{BlobHash: "dd11223344556677889900aabbccddeeff00112233445566778899aabbccddee",
			PackID: "01hzy3v7q8r9s0t1a2v3w4x5y7", Offset: 6, StoredLen: 64, RawLen: 64},
	}
	err := st.RecordPackedBlobs(rec, entries)
	require.Error(err)

	// The mismatch must fail the whole call: no index row was written.
	got, err := st.GetAttachmentPackEntry(entries[0].BlobHash)
	require.NoError(err)
	require.Nil(got)
}

func TestRecordPackedBlobsRejectsInvalidPackID(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	// Crockford base32 excludes "u", so this 26-character value is not a
	// canonical pack ULID even though its shape looks plausible.
	const invalidPackID = "01hzy3v7q8r9s0t1u2v3w4x5y6"
	hash := packTestHash("ee06")
	rec, entries := packTestRecord(invalidPackID, hash)

	err := st.RecordPackedBlobs(rec, entries)
	require.ErrorContains(err, "malformed pack id")

	has, err := st.HasPackRecord(invalidPackID)
	require.NoError(err)
	require.False(has, "invalid metadata must not be persisted")
	entry, err := st.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.Nil(entry, "invalid metadata must fail atomically")
}

func TestRecordAndAdoptPackedBlobsRejectInvalidMetadataAtomically(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store.PackRecord, []store.PackIndexEntry)
	}{
		{name: "negative offset", mutate: func(_ *store.PackRecord, entries []store.PackIndexEntry) {
			entries[0].Offset = -1
		}},
		{name: "negative stored length", mutate: func(_ *store.PackRecord, entries []store.PackIndexEntry) {
			entries[0].StoredLen = -1
		}},
		{name: "negative raw length", mutate: func(_ *store.PackRecord, entries []store.PackIndexEntry) {
			entries[0].RawLen = -1
		}},
		{name: "raw length beyond pack limit", mutate: func(_ *store.PackRecord, entries []store.PackIndexEntry) {
			entries[0].RawLen = int64(pack.MaxRawLen) + 1
		}},
		{name: "negative record entry count", mutate: func(rec *store.PackRecord, _ []store.PackIndexEntry) {
			rec.EntryCount = -1
		}},
		{name: "negative record stored bytes", mutate: func(rec *store.PackRecord, _ []store.PackIndexEntry) {
			rec.StoredBytes = -1
		}},
		{name: "submitted entry count exceeds record", mutate: func(rec *store.PackRecord, _ []store.PackIndexEntry) {
			rec.EntryCount = 0
		}},
		{name: "submitted stored bytes exceed record", mutate: func(rec *store.PackRecord, _ []store.PackIndexEntry) {
			rec.StoredBytes = 127
		}},
		{name: "nonhex hash", mutate: func(_ *store.PackRecord, entries []store.PackIndexEntry) {
			entries[0].BlobHash = packTestHash("zzzz")
		}},
		{name: "noncanonical uppercase hash", mutate: func(_ *store.PackRecord, entries []store.PackIndexEntry) {
			entries[0].BlobHash = strings.ToUpper(entries[0].BlobHash)
		}},
	}
	operations := []struct {
		name  string
		adopt bool
	}{
		{name: "record"},
		{name: "adopt", adopt: true},
	}

	for _, op := range operations {
		for _, tc := range tests {
			t.Run(op.name+"/"+tc.name, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				st := testutil.NewTestStore(t)
				fx := newPackAttachmentFixture(t, st)
				hash := packTestHash("ab07")
				legacyPath := "legacy/" + hash
				fx.addAttachment(hash, legacyPath, 100)

				const (
					oldPackID = "01hzy3v7q8r9s0t1a2v3w4x5a3"
					newPackID = "01hzy3v7q8r9s0t1a2v3w4x5a4"
				)
				if op.adopt {
					oldRec, oldEntries := packTestRecord(oldPackID, hash)
					require.NoError(st.RecordPackedBlobs(oldRec, oldEntries))
					_, err := st.DB().Exec(st.Rebind(`
						UPDATE attachments SET storage_path = ? WHERE content_hash = ?`),
						legacyPath, hash)
					require.NoError(err)
				}

				rec, entries := packTestRecord(newPackID, hash)
				tc.mutate(&rec, entries)
				var err error
				if op.adopt {
					err = st.AdoptPackedBlobs(rec, entries)
				} else {
					err = st.RecordPackedBlobs(rec, entries)
				}
				require.Error(err)

				has, getErr := st.HasPackRecord(newPackID)
				require.NoError(getErr)
				assert.False(has, "invalid input must not create a pack record")
				entry, getErr := st.GetAttachmentPackEntry(hash)
				require.NoError(getErr)
				if op.adopt {
					require.NotNil(entry)
					assert.Equal(oldPackID, entry.PackID,
						"invalid adoption must retain the old mapping")
				} else {
					assert.Nil(entry, "invalid record must not create a mapping")
				}
				assert.Equal([]string{legacyPath}, fx.pathsForContentHash(hash),
					"invalid input must not canonicalize paths")
			})
		}
	}
}

func TestAdoptPackedBlobsAllowsPartialPackMetadata(t *testing.T) {
	st := testutil.NewTestStore(t)
	hash := packTestHash("ac08")
	rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5a5", hash)
	rec.EntryCount = 2
	rec.StoredBytes = 256

	require.NoError(t, st.AdoptPackedBlobs(rec, entries),
		"orphan adoption may submit only the newly recovered subset")
}

func TestAdoptPackedBlobsRepointsExistingIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	hash := packTestHash("fa07")
	recA, entriesA := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5a1", hash)
	recB, entriesB := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5a2", hash)
	require.NoError(st.RecordPackedBlobs(recA, entriesA))

	require.NoError(st.AdoptPackedBlobs(recB, entriesB))

	entry, err := st.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	assert.Equal(recB.PackID, entry.PackID, "adoption transaction replaces unreadable source index")
	oldCount, err := st.CountPackIndexEntries(recA.PackID)
	require.NoError(err)
	assert.Zero(oldCount, "old pack remains recorded but no longer owns the rescued blob")
	newCount, err := st.CountPackIndexEntries(recB.PackID)
	require.NoError(err)
	assert.Equal(int64(1), newCount)
}

func TestAdoptPackedBlobsWithAliasesCanonicalizesLocalReferences(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	hash := packTestHash("fa08")
	uppercase := strings.ToUpper(hash)
	canonical := hash[:2] + "/" + hash
	contentURL := "HTTPS://cdn.example.com/" + uppercase
	thumbnailURL := "Http://cdn.example.com/" + uppercase

	fx.addAttachmentOnNewMessage(uppercase, "legacy/"+uppercase, 100)
	fx.addAttachmentOnNewMessage(uppercase, contentURL, 100)
	fx.addAttachmentOnNewMessage(uppercase, "", 100)
	carrierLocal := packTestHash("fa09")
	fx.addAttachment(carrierLocal, carrierLocal[:2]+"/"+carrierLocal, 100)
	fx.setThumbnail(carrierLocal, uppercase, "legacy/thumb/"+uppercase)
	carrierURL := packTestHash("fa0a")
	fx.addAttachment(carrierURL, carrierURL[:2]+"/"+carrierURL, 100)
	fx.setThumbnail(carrierURL, uppercase, thumbnailURL)
	carrierEmpty := packTestHash("fa0b")
	fx.addAttachment(carrierEmpty, carrierEmpty[:2]+"/"+carrierEmpty, 100)
	fx.setThumbnail(carrierEmpty, uppercase, "")

	rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5a6", hash)
	require.NoError(st.AdoptPackedBlobsWithAliases(rec, []store.PackIndexAdoption{{
		Entry:          entries[0],
		OriginalHashes: []string{uppercase},
	}}))

	assert.Equal([]string{canonical}, fx.pathsForContentHash(hash))
	assert.Equal([]string{canonical}, fx.thumbnailPathsForHash(hash))
	assert.ElementsMatch([]string{contentURL, ""}, fx.pathsForContentHash(uppercase),
		"URL-backed and empty content paths retain their original hash and path")
	assert.ElementsMatch([]string{thumbnailURL, ""}, fx.thumbnailPathsForHash(uppercase),
		"URL-backed and empty thumbnail paths retain their original hash and path")
	entry, err := st.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	assert.Equal(rec.PackID, entry.PackID)
	usage, err := st.ListPackUsage(context.Background())
	require.NoError(err)
	require.Len(usage, 1)
	assert.Equal(int64(1), usage[0].LiveEntries)
}

func TestCanonicalizeAttachmentBlobAliasesDeduplicatesCaseEquivalentRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	hash := packTestHash("fa0c")
	uppercase := strings.ToUpper(hash)
	canonical := hash[:2] + "/" + hash
	fx.addAttachment(uppercase, "legacy/"+uppercase, 100)
	fx.addAttachment(hash, "legacy/"+hash, 100)

	err := st.CanonicalizeAttachmentBlobAliases(hash, []string{uppercase, hash})
	require.NoError(err)

	var rows int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM attachments
		WHERE message_id = ? AND LOWER(content_hash) = ?`), fx.msgID, hash).Scan(&rows))
	assert.Equal(1, rows, "case-equivalent content rows are one logical attachment")
	assert.Equal([]string{canonical}, fx.pathsForContentHash(hash))
	assert.Empty(fx.pathsForContentHash(uppercase))
}

func TestCanonicalizeAttachmentBlobAliasesPreservesCaseEquivalentNonlocalRows(t *testing.T) {
	for _, tc := range []struct {
		name          string
		preservedPath string
	}{
		{name: "URL", preservedPath: "HTTPS://cdn.example.com/attachment"},
		{name: "empty path", preservedPath: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			fx := newPackAttachmentFixture(t, st)

			hash := packTestHash("fa0d")
			uppercase := strings.ToUpper(hash)
			canonical := hash[:2] + "/" + hash
			fx.addAttachment(hash, tc.preservedPath, 100)
			fx.addAttachment(uppercase, "legacy/"+uppercase, 100)

			err := st.CanonicalizeAttachmentBlobAliases(hash, []string{uppercase})
			require.NoError(err)

			rows, err := st.DB().Query(st.Rebind(`
				SELECT content_hash, storage_path FROM attachments
				WHERE message_id = ? AND LOWER(content_hash) = ? ORDER BY id`), fx.msgID, hash)
			require.NoError(err)
			defer func() { assert.NoError(rows.Close()) }()
			var got [][2]string
			for rows.Next() {
				var row [2]string
				require.NoError(rows.Scan(&row[0], &row[1]))
				got = append(got, row)
			}
			require.NoError(rows.Err())

			assert.Equal([][2]string{
				{uppercase, tc.preservedPath},
				{hash, canonical},
			}, got)
		})
	}
}

func TestAdoptPackedBlobsWithAliasesRejectsInvalidAliasesAtomically(t *testing.T) {
	tests := []struct {
		name  string
		alias string
	}{
		{name: "malformed", alias: "not-a-content-hash"},
		{name: "belongs to another entry", alias: strings.ToUpper(packTestHash("fb01"))},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			fx := newPackAttachmentFixture(t, st)

			hashA := packTestHash("fb01")
			hashB := packTestHash("fb02")
			uppercaseA := strings.ToUpper(hashA)
			uppercaseB := strings.ToUpper(hashB)
			legacyA := "legacy/" + uppercaseA
			legacyB := "legacy/" + uppercaseB
			fx.addAttachment(uppercaseA, legacyA, 100)
			fx.addAttachmentOnNewMessage(uppercaseB, legacyB, 100)
			rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5a7", hashA, hashB)

			err := st.AdoptPackedBlobsWithAliases(rec, []store.PackIndexAdoption{
				{Entry: entries[0], OriginalHashes: []string{uppercaseA}},
				{Entry: entries[1], OriginalHashes: []string{tc.alias}},
			})

			require.Error(err)
			has, getErr := st.HasPackRecord(rec.PackID)
			require.NoError(getErr)
			assert.False(has)
			for _, hash := range []string{hashA, hashB} {
				entry, entryErr := st.GetAttachmentPackEntry(hash)
				require.NoError(entryErr)
				assert.Nil(entry)
			}
			assert.Equal([]string{legacyA}, fx.pathsForContentHash(uppercaseA))
			assert.Equal([]string{legacyB}, fx.pathsForContentHash(uppercaseB))
		})
	}
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

func forceCaseSensitiveSQLiteLike(t *testing.T, st *store.Store) {
	t.Helper()
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		return
	}
	st.DB().SetMaxOpenConns(1)
	st.DB().SetMaxIdleConns(1)
	_, err := st.DB().Exec(`PRAGMA case_sensitive_like = ON`)
	require.NoError(t, err)
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

func (f *packAttachmentFixture) addAttachmentOnNewMessage(hash, path string, size int) {
	f.t.Helper()
	f.seq++
	src, err := f.store.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(f.t, err, "GetOrCreateSource")
	convID, err := f.store.EnsureConversation(src.ID, "thread-pack", "Pack Thread")
	require.NoError(f.t, err, "EnsureConversation")
	msgID, err := f.store.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: fmt.Sprintf("pack-msg-%d", f.seq),
		MessageType:     "email",
		SizeEstimate:    100,
	})
	require.NoError(f.t, err, "UpsertMessage")
	err = f.store.UpsertAttachment(msgID, fmt.Sprintf("file-%d.bin", f.seq),
		"application/octet-stream", path, hash, size)
	require.NoErrorf(f.t, err, "UpsertAttachment(%s)", hash)
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

func TestResolveAttachmentBlob(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	contentHash := packTestHash("a101")
	thumbnailHash := packTestHash("b102")
	unindexedHash := packTestHash("c103")
	staleHash := packTestHash("d104")
	fx.addAttachment(contentHash, contentHash[:2]+"/"+contentHash, 100)
	fx.setThumbnail(contentHash, thumbnailHash, thumbnailHash[:2]+"/"+thumbnailHash)
	fx.addAttachment(unindexedHash, unindexedHash[:2]+"/"+unindexedHash, 200)

	rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5v1",
		contentHash, thumbnailHash, staleHash)
	require.NoError(st.RecordPackedBlobs(rec, entries))

	for _, hash := range []string{contentHash, thumbnailHash} {
		loc, err := st.ResolveAttachmentBlob(hash)
		require.NoError(err)
		assert.True(loc.Referenced, "%s is referenced through content or thumbnail", hash)
		require.NotNil(loc.Pack)
		assert.Equal(hash, loc.Pack.BlobHash)
	}

	loc, err := st.ResolveAttachmentBlob(unindexedHash)
	require.NoError(err)
	assert.True(loc.Referenced)
	assert.Nil(loc.Pack, "referenced loose hash has no pack location")

	loc, err = st.ResolveAttachmentBlob(staleHash)
	require.NoError(err)
	assert.False(loc.Referenced, "a stale mapping is not a live attachment reference")
	assert.Nil(loc.Pack, "unreferenced mappings must not be exposed by production resolution")

	loc, err = st.ResolveAttachmentBlob(packTestHash("e105"))
	require.NoError(err)
	assert.False(loc.Referenced)
	assert.Nil(loc.Pack)
}

func TestResolveAttachmentBlobNormalizesPreservedCaseAliases(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(string) string
	}{
		{name: "URL only", path: func(hash string) string {
			return "HTTPS://cdn.example.com/" + hash
		}},
		{name: "empty path only", path: func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			forceCaseSensitiveSQLiteLike(t, st)
			fx := newPackAttachmentFixture(t, st)

			hash := packTestHash("a106")
			uppercase := strings.ToUpper(hash)
			preservedPath := tc.path(uppercase)
			fx.addAttachment(uppercase, preservedPath, 100)
			rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5v4", hash)
			require.NoError(st.RecordPackedBlobs(rec, entries))

			for _, requested := range []string{hash, uppercase} {
				loc, err := st.ResolveAttachmentBlob(requested)
				require.NoError(err)
				assert.True(loc.Referenced)
				require.NotNil(loc.Pack)
				assert.Equal(entries[0], *loc.Pack)
			}
			assert.Equal([]string{preservedPath}, fx.pathsForContentHash(uppercase))
			assert.Empty(fx.pathsForContentHash(hash))
		})
	}

	st := testutil.NewTestStore(t)
	_, err := st.ResolveAttachmentBlob("not-a-content-hash")
	require.ErrorContains(t, err, "malformed blob hash")
}

func TestPackIndexReadsRejectOutOfRangeScalars(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value int64
	}{
		{name: "negative offset", field: "pack_offset", value: -1},
		{name: "negative stored length", field: "stored_len", value: -1},
		{name: "negative raw length", field: "raw_len", value: -1},
		{name: "raw length beyond pack limit", field: "raw_len", value: int64(pack.MaxRawLen) + 1},
		{name: "negative flags", field: "flags", value: -1},
		{name: "flags beyond uint8", field: "flags", value: int64(^uint8(0)) + 1},
		{name: "negative crc32c", field: "crc32c", value: -1},
		{name: "crc32c beyond uint32", field: "crc32c", value: int64(^uint32(0)) + 1},
	}
	readers := []struct {
		name string
		read func(*store.Store, string, string) error
	}{
		{name: "get", read: func(st *store.Store, hash, _ string) error {
			_, err := st.GetAttachmentPackEntry(hash)
			return err
		}},
		{name: "resolve", read: func(st *store.Store, hash, _ string) error {
			_, err := st.ResolveAttachmentBlob(hash)
			return err
		}},
		{name: "list pack", read: func(st *store.Store, _, packID string) error {
			_, err := st.ListAttachmentPackEntries(packID)
			return err
		}},
		{name: "list referenced pack", read: func(st *store.Store, _, packID string) error {
			_, err := st.ListReferencedPackEntries(context.Background(), packID)
			return err
		}},
		{name: "list indexed", read: func(st *store.Store, _, _ string) error {
			_, err := st.ListIndexedBlobEntries()
			return err
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			fx := newPackAttachmentFixture(t, st)
			hash := packTestHash("ac01")
			const packID = "01hzy3v7q8r9s0t1a2v3w4x5t1"
			fx.addAttachment(hash, hash[:2]+"/"+hash, 100)
			rec, entries := packTestRecord(packID, hash)
			require.NoError(t, st.RecordPackedBlobs(rec, entries))

			_, err := st.DB().Exec(st.Rebind(fmt.Sprintf(
				"UPDATE attachment_pack_index SET %s = ? WHERE blob_hash = ?", tc.field,
			)), tc.value, hash)
			require.NoError(t, err)

			for _, reader := range readers {
				t.Run(reader.name, func(t *testing.T) {
					assert.Error(t, reader.read(st, hash, packID),
						"corrupt BIGINT metadata must be rejected before narrowing")
				})
			}
		})
	}
}

func TestPackIndexReadsRejectMalformedHashes(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		{name: "nonhex", hash: packTestHash("zzzz")},
		{name: "noncanonical uppercase", hash: strings.ToUpper(packTestHash("ab09"))},
	}
	readers := []struct {
		name string
		read func(*store.Store, string, string) error
	}{
		{name: "get", read: func(st *store.Store, hash, _ string) error {
			_, err := st.GetAttachmentPackEntry(hash)
			return err
		}},
		{name: "resolve", read: func(st *store.Store, hash, _ string) error {
			_, err := st.ResolveAttachmentBlob(hash)
			return err
		}},
		{name: "list pack", read: func(st *store.Store, _, packID string) error {
			_, err := st.ListAttachmentPackEntries(packID)
			return err
		}},
		{name: "list referenced pack", read: func(st *store.Store, _, packID string) error {
			_, err := st.ListReferencedPackEntries(context.Background(), packID)
			return err
		}},
		{name: "list indexed", read: func(st *store.Store, _, _ string) error {
			_, err := st.ListIndexedBlobEntries()
			return err
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			fx := newPackAttachmentFixture(t, st)
			fx.addAttachment(tc.hash, "legacy/"+tc.hash, 100)
			const packID = "01hzy3v7q8r9s0t1a2v3w4x5t2"
			_, err := st.DB().Exec(st.Rebind(`
				INSERT INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
				VALUES (?, 1, 128, ?)`), packID, time.Now().UTC().Format(time.RFC3339))
			require.NoError(t, err)
			_, err = st.DB().Exec(st.Rebind(`
				INSERT INTO attachment_pack_index
				    (blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
				VALUES (?, ?, 6, 128, 256, 0, 0)`), tc.hash, packID)
			require.NoError(t, err)

			for _, reader := range readers {
				t.Run(reader.name, func(t *testing.T) {
					assert.Error(t, reader.read(st, tc.hash, packID),
						"legacy corrupt hash must be reported, not normalized")
				})
			}
		})
	}
}

func TestListReferencedBlobHashes(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	contentHash := packTestHash("a111")
	thumbnailHash := packTestHash("b112")
	sharedAcrossColumns := packTestHash("c113")
	fx.addAttachment(contentHash, contentHash[:2]+"/"+contentHash, 100)
	fx.setThumbnail(contentHash, thumbnailHash, thumbnailHash[:2]+"/"+thumbnailHash)
	fx.addAttachment(sharedAcrossColumns, sharedAcrossColumns[:2]+"/"+sharedAcrossColumns, 100)
	fx.setThumbnail(sharedAcrossColumns, sharedAcrossColumns, sharedAcrossColumns[:2]+"/"+sharedAcrossColumns)

	hashes, err := st.ListReferencedBlobHashes()
	require.NoError(err)
	assert.Equal(t, map[string]struct{}{
		contentHash: {}, thumbnailHash: {}, sharedAcrossColumns: {},
	}, hashes)
}

func TestPruneUnreferencedPackIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	contentHash := packTestHash("a121")
	thumbnailHash := packTestHash("b122")
	staleA := packTestHash("c123")
	staleB := packTestHash("d124")
	fx.addAttachment(contentHash, contentHash[:2]+"/"+contentHash, 100)
	fx.setThumbnail(contentHash, thumbnailHash, thumbnailHash[:2]+"/"+thumbnailHash)
	rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5v2",
		contentHash, thumbnailHash, staleA, staleB)
	require.NoError(st.RecordPackedBlobs(rec, entries))

	pruned, err := st.PruneUnreferencedPackIndex(context.Background())
	require.NoError(err)
	assert.Equal(int64(2), pruned)

	indexed, err := st.ListIndexedBlobHashes()
	require.NoError(err)
	assert.Equal(map[string]struct{}{contentHash: {}, thumbnailHash: {}}, indexed)

	pruned, err = st.PruneUnreferencedPackIndex(context.Background())
	require.NoError(err)
	assert.Zero(pruned, "repair is idempotent")
}

func TestPruneUnreferencedPackIndexPreservesCaseAliasReference(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	hash := packTestHash("a125")
	uppercase := strings.ToUpper(hash)
	fx.addAttachment(uppercase, "legacy/"+uppercase, 100)
	rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5v3", hash)
	require.NoError(st.RecordPackedBlobs(rec, entries))

	pruned, err := st.PruneUnreferencedPackIndex(context.Background())

	require.NoError(err)
	assert.Zero(pruned, "case aliases must preserve an otherwise-live packed mapping")
	entry, err := st.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	assert.Equal(rec.PackID, entry.PackID)
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

func (f *packAttachmentFixture) thumbnailPathsForHash(thumbHash string) []string {
	f.t.Helper()
	rows, err := f.store.DB().Query(f.store.Rebind(`
		SELECT thumbnail_path FROM attachments WHERE thumbnail_hash = ? ORDER BY id`), thumbHash)
	require.NoErrorf(f.t, err, "query thumbnail_path for %s", thumbHash)
	defer rows.Close() //nolint:errcheck // read-only cursor
	var paths []string
	for rows.Next() {
		var path string
		require.NoError(f.t, rows.Scan(&path), "scan thumbnail_path")
		paths = append(paths, path)
	}
	require.NoError(f.t, rows.Err(), "rows.Err")
	return paths
}

func TestCanonicalizeAttachmentBlobPaths(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	hash := packTestHash("ab01")
	canonical := hash[:2] + "/" + hash
	for _, path := range []string{
		"legacy/one/" + hash,
		"legacy/two/" + hash,
		canonical,
		"https://cdn.example.com/" + hash,
		"HTTP://cdn.example.com/" + hash,
		"",
	} {
		fx.addAttachmentOnNewMessage(hash, path, 100)
	}
	thumbnailPaths := []string{
		"legacy/thumb-one/" + hash,
		"legacy/thumb-two/" + hash,
		canonical,
		"http://cdn.example.com/" + hash,
		"Https://cdn.example.com/" + hash,
		"",
	}
	for i, path := range thumbnailPaths {
		contentHash := packTestHash(fmt.Sprintf("c%03x", i))
		fx.addAttachment(contentHash, contentHash[:2]+"/"+contentHash, 100)
		fx.setThumbnail(contentHash, hash, path)
	}

	require.NoError(st.CanonicalizeAttachmentBlobPaths(strings.ToUpper(hash)))

	assert.Equal([]string{
		canonical,
		canonical,
		canonical,
		"https://cdn.example.com/" + hash,
		"HTTP://cdn.example.com/" + hash,
		"",
	}, fx.pathsForContentHash(hash))
	assert.Equal([]string{
		canonical,
		canonical,
		canonical,
		"http://cdn.example.com/" + hash,
		"Https://cdn.example.com/" + hash,
		"",
	}, fx.thumbnailPathsForHash(hash))
}

func TestCanonicalizeAttachmentBlobPathsPreservesURLsWithCaseSensitiveLike(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	st.DB().SetMaxOpenConns(1)
	st.DB().SetMaxIdleConns(1)
	_, err := st.DB().Exec(`PRAGMA case_sensitive_like = ON`)
	require.NoError(err)
	fx := newPackAttachmentFixture(t, st)

	hash := packTestHash("ab0a")
	contentURL := "HTTP://cdn.example.com/" + hash
	thumbnailURL := "Https://cdn.example.com/" + hash
	fx.addAttachment(hash, contentURL, 100)
	carrierHash := packTestHash("ab0b")
	fx.addAttachment(carrierHash, carrierHash[:2]+"/"+carrierHash, 100)
	fx.setThumbnail(carrierHash, hash, thumbnailURL)

	require.NoError(st.CanonicalizeAttachmentBlobPaths(hash))
	assert.Equal([]string{contentURL}, fx.pathsForContentHash(hash))
	assert.Equal([]string{thumbnailURL}, fx.thumbnailPathsForHash(hash))
}

func TestCanonicalizeAttachmentBlobPathsNormalizesUppercaseStoredHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)
	attachmentsDir := t.TempDir()

	content := []byte("uppercase content hash loose read")
	thumbnail := []byte("uppercase thumbnail hash loose read")
	contentHash := pack.ComputeBlobID(content).String()
	thumbnailHash := pack.ComputeBlobID(thumbnail).String()
	uppercaseContentHash := strings.ToUpper(contentHash)
	uppercaseThumbnailHash := strings.ToUpper(thumbnailHash)
	contentPath := contentHash[:2] + "/" + contentHash
	thumbnailPath := thumbnailHash[:2] + "/" + thumbnailHash
	contentURL := "HTTPS://cdn.example.com/" + uppercaseContentHash
	thumbnailURL := "Http://cdn.example.com/" + uppercaseThumbnailHash

	fx.addAttachmentOnNewMessage(uppercaseContentHash, "legacy/"+uppercaseContentHash, len(content))
	fx.addAttachmentOnNewMessage(uppercaseContentHash, contentURL, len(content))
	fx.addAttachmentOnNewMessage(uppercaseContentHash, "", len(content))
	carrierLocal := packTestHash("ab0d")
	fx.addAttachment(carrierLocal, carrierLocal[:2]+"/"+carrierLocal, 100)
	fx.setThumbnail(carrierLocal, uppercaseThumbnailHash, "legacy/thumb/"+uppercaseThumbnailHash)
	carrierURL := packTestHash("ab0e")
	fx.addAttachment(carrierURL, carrierURL[:2]+"/"+carrierURL, 100)
	fx.setThumbnail(carrierURL, uppercaseThumbnailHash, thumbnailURL)
	carrierEmpty := packTestHash("ab0f")
	fx.addAttachment(carrierEmpty, carrierEmpty[:2]+"/"+carrierEmpty, 100)
	fx.setThumbnail(carrierEmpty, uppercaseThumbnailHash, "")

	for path, data := range map[string][]byte{
		contentPath:   content,
		thumbnailPath: thumbnail,
	} {
		fullPath := filepath.Join(attachmentsDir, filepath.FromSlash(path))
		require.NoError(os.MkdirAll(filepath.Dir(fullPath), 0o700))
		require.NoError(os.WriteFile(fullPath, data, 0o600))
	}

	require.NoError(st.CanonicalizeAttachmentBlobPaths(uppercaseContentHash))
	require.NoError(st.CanonicalizeAttachmentBlobPaths(uppercaseThumbnailHash))

	var storedContentHash, storedContentPath string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT content_hash, storage_path FROM attachments WHERE storage_path = ?`),
		contentPath).Scan(&storedContentHash, &storedContentPath))
	assert.Equal(contentHash, storedContentHash)
	assert.Equal(contentPath, storedContentPath)
	var storedThumbnailHash, storedThumbnailPath string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT thumbnail_hash, thumbnail_path FROM attachments WHERE thumbnail_path = ?`),
		thumbnailPath).Scan(&storedThumbnailHash, &storedThumbnailPath))
	assert.Equal(thumbnailHash, storedThumbnailHash)
	assert.Equal(thumbnailPath, storedThumbnailPath)

	var preserved int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM attachments
		WHERE content_hash = ? AND (storage_path = ? OR storage_path = '')`),
		uppercaseContentHash, contentURL).Scan(&preserved))
	assert.Equal(2, preserved, "content URL and empty rows retain their original hash and path")
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM attachments
		WHERE thumbnail_hash = ? AND (thumbnail_path = ? OR thumbnail_path = '')`),
		uppercaseThumbnailHash, thumbnailURL).Scan(&preserved))
	assert.Equal(2, preserved, "thumbnail URL and empty rows retain their original hash and path")

	loose := blobstore.New(st, attachmentsDir)
	t.Cleanup(func() { require.NoError(loose.Close()) })
	for _, tc := range []struct {
		hash string
		want []byte
	}{
		{hash: storedContentHash, want: content},
		{hash: storedThumbnailHash, want: thumbnail},
	} {
		reader, size, err := loose.Open(tc.hash)
		require.NoError(err)
		got, err := io.ReadAll(reader)
		require.NoError(err)
		require.NoError(reader.Close())
		assert.Equal(int64(len(tc.want)), size)
		assert.Equal(tc.want, got)
	}
}

func TestCanonicalizeAttachmentBlobPathsRejectsMalformedHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	malformed := packTestHash("zzzz")
	path := "legacy/" + malformed
	fx.addAttachment(malformed, path, 100)

	err := st.CanonicalizeAttachmentBlobPaths(malformed)
	require.ErrorContains(err, "malformed blob hash")
	assert.Equal([]string{path}, fx.pathsForContentHash(malformed),
		"validation failure leaves paths untouched")
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

	rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5z1",
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

	rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5z2", hashGood)
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

	rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5z3", hashPacked)
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
		Hash: hashLoose, OriginalHashes: []string{hashLoose},
		Paths: []string{hashLoose[:2] + "/" + hashLoose}, Size: 100,
	}, byHash[hashLoose])

	require.Contains(byHash, hashBoth)
	assert.Equal(int64(300), byHash[hashBoth].Size,
		"content-vs-thumbnail dup keeps the content row (with size)")
	assert.Equal([]string{hashBoth[:2] + "/" + hashBoth, "thumbs/" + hashBoth},
		byHash[hashBoth].Paths, "all content and thumbnail candidate paths are retained")

	require.Contains(byHash, hashThumbOnly)
	assert.Equal(store.UnpackedBlob{
		Hash: hashThumbOnly, OriginalHashes: []string{hashThumbOnly},
		Paths: []string{"thumbs/" + hashThumbOnly}, Size: -1,
	}, byHash[hashThumbOnly], "thumbnail-only blobs have Size -1")

	assert.Len(blobs, 3)
}

func TestListUnpackedBlobsCoalescesCaseAliasesDeterministically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	forceCaseSensitiveSQLiteLike(t, st)
	fx := newPackAttachmentFixture(t, st)

	shared := packTestHash("ab2d")
	uppercase := strings.ToUpper(shared)
	unrelated := packTestHash("cd2e")
	fx.addAttachment(uppercase, "legacy/"+uppercase, 100)
	fx.addAttachmentOnNewMessage(shared, shared[:2]+"/"+shared, 300)
	fx.addAttachmentOnNewMessage(unrelated, unrelated[:2]+"/"+unrelated, 200)
	fx.addAttachmentOnNewMessage("BAD-HASH", "malformed/BAD-HASH", 50)
	fx.addAttachmentOnNewMessage("bad-hash", "malformed/bad-hash", 60)
	fx.setThumbnail(unrelated, uppercase, "thumbs/"+uppercase)

	blobs, err := st.ListUnpackedBlobs()

	require.NoError(err)
	require.Len(blobs, 4, "valid case aliases coalesce while malformed spellings remain distinct")
	assert.Equal(shared, blobs[0].Hash)
	assert.Equal([]string{uppercase, shared}, blobs[0].OriginalHashes,
		"original local spellings remain ordered for atomic canonicalization")
	assert.Equal([]string{"legacy/" + uppercase, shared[:2] + "/" + shared, "thumbs/" + uppercase},
		blobs[0].Paths)
	assert.Equal(int64(300), blobs[0].Size, "content aliases retain the largest recorded size")
	assert.Equal(unrelated, blobs[1].Hash, "content-first first-seen order remains deterministic")
	assert.Equal(store.UnpackedBlob{
		Hash: "BAD-HASH", OriginalHashes: []string{"BAD-HASH"},
		Paths: []string{"malformed/BAD-HASH"}, Size: 50,
	}, blobs[2])
	assert.Equal(store.UnpackedBlob{
		Hash: "bad-hash", OriginalHashes: []string{"bad-hash"},
		Paths: []string{"malformed/bad-hash"}, Size: 60,
	}, blobs[3])
}

func TestListUnpackedBlobsExcludesPackedCaseAliases(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	forceCaseSensitiveSQLiteLike(t, st)
	fx := newPackAttachmentFixture(t, st)

	contentHash := packTestHash("aa2a")
	thumbnailHash := packTestHash("bb2b")
	uppercaseContent := strings.ToUpper(contentHash)
	uppercaseThumbnail := strings.ToUpper(thumbnailHash)
	fx.addAttachment(uppercaseContent, "legacy/"+uppercaseContent, 100)
	carrierHash := packTestHash("cc2c")
	fx.addAttachment(carrierHash, carrierHash[:2]+"/"+carrierHash, 100)
	fx.setThumbnail(carrierHash, uppercaseThumbnail, "legacy/thumb/"+uppercaseThumbnail)
	rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5z6", contentHash, thumbnailHash)
	require.NoError(st.RecordPackedBlobs(rec, entries))

	blobs, err := st.ListUnpackedBlobs()

	require.NoError(err)
	byHash := make(map[string]store.UnpackedBlob, len(blobs))
	for _, blob := range blobs {
		byHash[blob.Hash] = blob
	}
	assert.NotContains(byHash, uppercaseContent)
	assert.NotContains(byHash, uppercaseThumbnail)
}

func TestListUnpackedBlobsExcludesURLsWithCaseSensitiveLike(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	st.DB().SetMaxOpenConns(1)
	st.DB().SetMaxIdleConns(1)
	_, err := st.DB().Exec(`PRAGMA case_sensitive_like = ON`)
	require.NoError(err)
	fx := newPackAttachmentFixture(t, st)

	contentURLHash := packTestHash("aa27")
	thumbnailURLHash := packTestHash("bb28")
	fx.addAttachment(contentURLHash, "HTTPS://cdn.example.com/"+contentURLHash, 100)
	carrierHash := packTestHash("cc29")
	fx.addAttachment(carrierHash, carrierHash[:2]+"/"+carrierHash, 100)
	fx.setThumbnail(carrierHash, thumbnailURLHash, "Http://cdn.example.com/"+thumbnailURLHash)

	blobs, err := st.ListUnpackedBlobs()
	require.NoError(err)
	byHash := make(map[string]store.UnpackedBlob, len(blobs))
	for _, blob := range blobs {
		byHash[blob.Hash] = blob
	}
	assert.NotContains(byHash, contentURLHash)
	assert.NotContains(byHash, thumbnailURLHash)
}

func TestPackRecordLifecycle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	hashA := packTestHash("aa31")
	hashB := packTestHash("bb32")
	hashC := packTestHash("cc33")

	recA, entriesA := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5z4", hashA, hashB)
	recB, entriesB := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5z5", hashC)
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
	has, err = st.HasPackRecord("01hzy3v7q8r9s0t1a2v3w4x5z9")
	require.NoError(err)
	assert.False(has)

	n, err := st.CountPackIndexEntries(recA.PackID)
	require.NoError(err)
	assert.Equal(int64(2), n)
	n, err = st.CountPackIndexEntries("01hzy3v7q8r9s0t1a2v3w4x5z9")
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

func TestListIndexedBlobEntries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	hashA := packTestHash("ad31")
	hashB := packTestHash("be32")
	rec, entries := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5r1", hashA, hashB)
	entries[0].Flags = 1
	entries[0].CRC32C = 4022250974
	require.NoError(st.RecordPackedBlobs(rec, entries))

	indexed, err := st.ListIndexedBlobEntries()
	require.NoError(err)
	assert.Equal(map[string]store.PackIndexEntry{
		hashA: entries[0],
		hashB: entries[1],
	}, indexed, "sweep metadata includes every referenced or stale index row")
}

func TestClearAttachmentPackMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	recA, entriesA := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5w1",
		packTestHash("aa41"), packTestHash("bb42"))
	recB, entriesB := packTestRecord("01hzy3v7q8r9s0t1a2v3w4x5w2",
		packTestHash("cc43"))
	require.NoError(st.RecordPackedBlobs(recA, entriesA))
	require.NoError(st.RecordPackedBlobs(recB, entriesB))

	require.NoError(st.ClearAttachmentPackMetadata())

	recs, err := st.ListPackRecords()
	require.NoError(err)
	assert.Empty(recs, "attachment_packs cleared")
	hashes, err := st.ListIndexedBlobHashes()
	require.NoError(err)
	assert.Empty(hashes, "attachment_pack_index cleared")
	entry, err := st.GetAttachmentPackEntry(packTestHash("aa41"))
	require.NoError(err)
	assert.Nil(entry, "cleared blob reads as unpacked")

	// Idempotent on an already-empty state (restore of an unpacked vault).
	require.NoError(st.ClearAttachmentPackMetadata())
}
