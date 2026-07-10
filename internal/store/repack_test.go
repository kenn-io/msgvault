package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPackUsageIsReferenceAwareAndDeterministic(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	const (
		packA = "01hzy3v7q8r9s0t1a2v3w4x6a1"
		packB = "01hzy3v7q8r9s0t1a2v3w4x6b1"
		packC = "01hzy3v7q8r9s0t1a2v3w4x6c1"
	)
	liveA := packTestHash("a801")
	liveA2 := packTestHash("a803")
	staleA := packTestHash("a802")
	liveB := packTestHash("b801")
	staleC := packTestHash("c802")
	fx.addAttachment(liveA, liveA[:2]+"/"+liveA, 111)
	fx.addAttachment(liveA2, liveA2[:2]+"/"+liveA2, 90)
	fx.addAttachment(liveB, liveB[:2]+"/"+liveB, 222)

	created := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	require.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID: packB, EntryCount: 1, StoredBytes: 70, CreatedAt: created,
	}, []store.PackIndexEntry{{
		BlobHash: liveB, PackID: packB, Offset: 6, StoredLen: 70, RawLen: 222,
	}}))
	require.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID: packA, EntryCount: 3, StoredBytes: 120, CreatedAt: created,
	}, []store.PackIndexEntry{
		{BlobHash: liveA, PackID: packA, Offset: 6, StoredLen: 40, RawLen: 111},
		{BlobHash: liveA2, PackID: packA, Offset: 46, StoredLen: 20, RawLen: 90},
		{BlobHash: staleA, PackID: packA, Offset: 66, StoredLen: 60, RawLen: 333},
	}))
	require.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID: packC, EntryCount: 1, StoredBytes: 80, CreatedAt: created,
	}, []store.PackIndexEntry{
		{BlobHash: staleC, PackID: packC, Offset: 6, StoredLen: 80, RawLen: 444},
	}))

	usage, err := st.ListPackUsage(context.Background())
	require.NoError(err)
	require.Len(usage, 3)
	assert.Equal(packA, usage[0].PackID, "equal timestamps order by pack ID")
	assert.Equal(int64(2), usage[0].LiveEntries)
	assert.Equal(int64(60), usage[0].LiveStoredBytes)
	assert.Equal(int64(201), usage[0].LiveRawBytes)
	assert.Equal(int64(40), usage[0].MaxLiveStoredLen,
		"larger dead entry is excluded from the referenced maximum")
	assert.Equal(int64(111), usage[0].MaxLiveRawLen,
		"larger dead entry is excluded from the referenced maximum")
	assert.Equal(packB, usage[1].PackID)
	assert.Equal(int64(70), usage[1].MaxLiveStoredLen)
	assert.Equal(int64(222), usage[1].MaxLiveRawLen)
	assert.Equal(packC, usage[2].PackID)
	assert.Zero(usage[2].LiveEntries)
	assert.Zero(usage[2].MaxLiveStoredLen, "zero-live pack has no referenced maximum")
	assert.Zero(usage[2].MaxLiveRawLen, "zero-live pack has no referenced maximum")

	entries, err := st.ListReferencedPackEntries(context.Background(), packA)
	require.NoError(err)
	require.Len(entries, 2)
	assert.Equal(liveA, entries[0].BlobHash)
	assert.Equal(liveA2, entries[1].BlobHash)
}

func TestPackMaintenanceUsesPreservedCaseAliasesForLiveness(t *testing.T) {
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

			const (
				oldPack = "01hzy3v7q8r9s0t1a2v3w4x6a2"
				newPack = "01hzy3v7q8r9s0t1a2v3w4x6a3"
			)
			hash := packTestHash("a804")
			uppercase := strings.ToUpper(hash)
			preservedPath := tc.path(uppercase)
			fx.addAttachment(uppercase, preservedPath, 100)
			created := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
			require.NoError(st.RecordPackedBlobs(store.PackRecord{
				PackID: oldPack, EntryCount: 1, StoredBytes: 80, CreatedAt: created,
			}, []store.PackIndexEntry{{
				BlobHash: hash, PackID: oldPack, Offset: 6, StoredLen: 80, RawLen: 100,
			}}))

			usage, err := st.ListPackUsage(context.Background())
			require.NoError(err)
			require.Len(usage, 1)
			assert.Equal(int64(1), usage[0].LiveEntries)
			entries, err := st.ListReferencedPackEntries(context.Background(), oldPack)
			require.NoError(err)
			require.Len(entries, 1)
			assert.Equal(hash, entries[0].BlobHash)
			deleted, err := st.DeleteEmptyPackRecord(context.Background(), oldPack)
			require.NoError(err)
			assert.False(deleted, "uppercase alias keeps the source pack live")

			require.NoError(st.CommitRepack(context.Background(), []string{oldPack}, []store.PackRecord{{
				PackID: newPack, EntryCount: 1, StoredBytes: 50, CreatedAt: created.Add(time.Hour),
			}}, []store.RepackMove{{
				OldPackID: oldPack,
				NewEntry: store.PackIndexEntry{
					BlobHash: hash, PackID: newPack, Offset: 6, StoredLen: 50, RawLen: 100,
				},
			}}))

			entry, err := st.GetAttachmentPackEntry(hash)
			require.NoError(err)
			require.NotNil(entry)
			assert.Equal(newPack, entry.PackID)
			entries, err = st.ListReferencedPackEntries(context.Background(), newPack)
			require.NoError(err)
			assert.Len(entries, 1)
			assert.Equal([]string{preservedPath}, fx.pathsForContentHash(uppercase),
				"repack metadata must not rewrite external attachment policy")
			assert.Empty(fx.pathsForContentHash(hash))
		})
	}
}

func TestCommitRepackKeepsCanonicalIndexCASExactForCaseAliases(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	forceCaseSensitiveSQLiteLike(t, st)
	fx := newPackAttachmentFixture(t, st)

	const (
		oldPack = "01hzy3v7q8r9s0t1a2v3w4x6a4"
		newPack = "01hzy3v7q8r9s0t1a2v3w4x6a5"
	)
	hash := packTestHash("a805")
	uppercase := strings.ToUpper(hash)
	fx.addAttachment(uppercase, "HTTPS://cdn.example.com/"+uppercase, 100)
	created := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	require.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID: oldPack, EntryCount: 1, StoredBytes: 80, CreatedAt: created,
	}, []store.PackIndexEntry{{
		BlobHash: hash, PackID: oldPack, Offset: 6, StoredLen: 80, RawLen: 100,
	}}))

	err := st.CommitRepack(context.Background(), []string{oldPack}, []store.PackRecord{{
		PackID: newPack, EntryCount: 1, StoredBytes: 50, CreatedAt: created.Add(time.Hour),
	}}, []store.RepackMove{{
		OldPackID: oldPack,
		NewEntry: store.PackIndexEntry{
			BlobHash: uppercase, PackID: newPack, Offset: 6, StoredLen: 50, RawLen: 100,
		},
	}})

	require.ErrorContains(err, "exact")
	entry, getErr := st.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.Equal(oldPack, entry.PackID)
	has, getErr := st.HasPackRecord(newPack)
	require.NoError(getErr)
	assert.False(has, "case-mismatched CAS must roll back the output record")
}

func TestPackUsageRejectsImpossibleAccounting(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)
	hash := packTestHash("c801")
	fx.addAttachment(hash, hash[:2]+"/"+hash, 100)

	const packID = "01hzy3v7q8r9s0t1a2v3w4x6c1"
	_, err := st.DB().Exec(st.Rebind(`
		INSERT INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
		VALUES (?, 0, 1, ?)`), packID, time.Now().UTC().Format(time.RFC3339))
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO attachment_pack_index
		    (blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
		VALUES (?, ?, 6, 2, 100, 0, 0)`), hash, packID)
	require.NoError(err)

	_, err = st.ListPackUsage(context.Background())
	require.ErrorContains(err, "impossible accounting")
}

func TestPackUsageRejectsNegativeTotals(t *testing.T) {
	for _, tc := range []struct {
		name        string
		entryCount  int64
		storedBytes int64
	}{
		{name: "entry count", entryCount: -1, storedBytes: 0},
		{name: "stored bytes", entryCount: 0, storedBytes: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			_, err := st.DB().Exec(st.Rebind(`
				INSERT INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
				VALUES (?, ?, ?, ?)`), "01hzy3v7q8r9s0t1a2v3w4x6c2", tc.entryCount,
				tc.storedBytes, time.Now().UTC().Format(time.RFC3339))
			require.NoError(t, err)

			_, err = st.ListPackUsage(context.Background())
			require.ErrorContains(t, err, "impossible accounting")
		})
	}
}

func TestPackUsageRejectsInconsistentMaxima(t *testing.T) {
	for _, field := range []string{"stored_len", "raw_len"} {
		t.Run(field, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			fx := newPackAttachmentFixture(t, st)
			hashA := packTestHash("ca01")
			hashB := packTestHash("ca02")
			fx.addAttachment(hashA, hashA[:2]+"/"+hashA, 10)
			fx.addAttachment(hashB, hashB[:2]+"/"+hashB, 10)
			const packID = "01hzy3v7q8r9s0t1a2v3w4x6c3"
			_, err := st.DB().Exec(st.Rebind(`
				INSERT INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
				VALUES (?, 2, 20, ?)`), packID, time.Now().UTC().Format(time.RFC3339))
			require.NoError(t, err)
			for i, hash := range []string{hashA, hashB} {
				storedLen, rawLen := int64(10), int64(10)
				if field == "stored_len" && i == 1 {
					storedLen = -9
				}
				if field == "raw_len" && i == 1 {
					rawLen = -9
				}
				_, err = st.DB().Exec(st.Rebind(`
					INSERT INTO attachment_pack_index
					    (blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
					VALUES (?, ?, ?, ?, ?, 0, 0)`), hash, packID, 6+i*10, storedLen, rawLen)
				require.NoError(t, err)
			}

			_, err = st.ListPackUsage(context.Background())
			require.ErrorContains(t, err, "impossible accounting",
				"a maximum cannot exceed its nonnegative aggregate")
		})
	}
}

func TestPackUsageRejectsRawMaximumBeyondPackLimit(t *testing.T) {
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)
	hash := packTestHash("ca03")
	fx.addAttachment(hash, hash[:2]+"/"+hash, 10)
	const packID = "01hzy3v7q8r9s0t1a2v3w4x6c4"
	_, err := st.DB().Exec(st.Rebind(`
		INSERT INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
		VALUES (?, 1, 10, ?)`), packID, time.Now().UTC().Format(time.RFC3339))
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO attachment_pack_index
		    (blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
		VALUES (?, ?, 6, 10, ?, 0, 0)`), hash, packID, int64(pack.MaxRawLen)+1)
	require.NoError(t, err)

	_, err = st.ListPackUsage(context.Background())
	require.ErrorContains(t, err, "impossible accounting")
}

func TestCommitRepackSwapsAllSelectedMappingsAtomically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	const (
		oldA = "01hzy3v7q8r9s0t1a2v3w4x6d1"
		oldB = "01hzy3v7q8r9s0t1a2v3w4x6d2"
		newA = "01hzy3v7q8r9s0t1a2v3w4x6d3"
	)
	hashA := packTestHash("d801")
	hashB := packTestHash("d802")
	fx.addAttachment(hashA, hashA[:2]+"/"+hashA, 100)
	fx.addAttachment(hashB, hashB[:2]+"/"+hashB, 200)
	created := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	require.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID: oldA, EntryCount: 1, StoredBytes: 80, CreatedAt: created,
	}, []store.PackIndexEntry{{BlobHash: hashA, PackID: oldA, Offset: 6, StoredLen: 80, RawLen: 100}}))
	require.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID: oldB, EntryCount: 1, StoredBytes: 90, CreatedAt: created,
	}, []store.PackIndexEntry{{BlobHash: hashB, PackID: oldB, Offset: 6, StoredLen: 90, RawLen: 200}}))

	record := store.PackRecord{PackID: newA, EntryCount: 2, StoredBytes: 120, CreatedAt: created.Add(time.Hour)}
	moves := []store.RepackMove{
		{OldPackID: oldA, NewEntry: store.PackIndexEntry{BlobHash: hashA, PackID: newA, Offset: 6, StoredLen: 50, RawLen: 100}},
		{OldPackID: oldB, NewEntry: store.PackIndexEntry{BlobHash: hashB, PackID: newA, Offset: 56, StoredLen: 70, RawLen: 200}},
	}
	require.NoError(st.CommitRepack(context.Background(), []string{oldA, oldB}, []store.PackRecord{record}, moves))

	for _, hash := range []string{hashA, hashB} {
		entry, err := st.GetAttachmentPackEntry(hash)
		require.NoError(err)
		require.NotNil(entry)
		assert.Equal(newA, entry.PackID)
	}
	has, err := st.HasPackRecord(newA)
	require.NoError(err)
	assert.True(has)
}

func TestCommitRepackRejectsWhollyOmittedSelectedPack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	const (
		oldA = "01hzy3v7q8r9s0t1a2v3w4x6e1"
		oldB = "01hzy3v7q8r9s0t1a2v3w4x6e2"
		newA = "01hzy3v7q8r9s0t1a2v3w4x6e3"
	)
	hashA := packTestHash("e801")
	hashB := packTestHash("e802")
	fx.addAttachment(hashA, hashA[:2]+"/"+hashA, 100)
	fx.addAttachment(hashB, hashB[:2]+"/"+hashB, 200)
	created := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	require.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID: oldA, EntryCount: 1, StoredBytes: 80, CreatedAt: created,
	}, []store.PackIndexEntry{{BlobHash: hashA, PackID: oldA, Offset: 6, StoredLen: 80, RawLen: 100}}))
	require.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID: oldB, EntryCount: 1, StoredBytes: 90, CreatedAt: created,
	}, []store.PackIndexEntry{{BlobHash: hashB, PackID: oldB, Offset: 6, StoredLen: 90, RawLen: 200}}))

	err := st.CommitRepack(context.Background(), []string{oldA, oldB}, []store.PackRecord{{
		PackID: newA, EntryCount: 1, StoredBytes: 50, CreatedAt: created.Add(time.Hour),
	}}, []store.RepackMove{{
		OldPackID: oldA,
		NewEntry:  store.PackIndexEntry{BlobHash: hashA, PackID: newA, Offset: 6, StoredLen: 50, RawLen: 100},
	}})
	require.ErrorContains(err, "exact")

	for hash, wantPack := range map[string]string{hashA: oldA, hashB: oldB} {
		entry, getErr := st.GetAttachmentPackEntry(hash)
		require.NoError(getErr)
		require.NotNil(entry)
		assert.Equal(wantPack, entry.PackID)
	}
	has, err := st.HasPackRecord(newA)
	require.NoError(err)
	assert.False(has, "failed swap must roll back new pack records")
}

func TestCommitRepackRejectsChangedExpectedMappingSets(t *testing.T) {
	const (
		oldA = "01hzy3v7q8r9s0t1a2v3w4x6g1"
		oldB = "01hzy3v7q8r9s0t1a2v3w4x6g2"
		newA = "01hzy3v7q8r9s0t1a2v3w4x6g3"
	)
	tests := []struct {
		name   string
		mutate func(t *testing.T, st *store.Store, hashA, hashB string)
		moves  func(hashA, hashB string) []store.RepackMove
	}{
		{
			name: "missing current mapping",
			mutate: func(t *testing.T, st *store.Store, _ string, hashB string) {
				t.Helper()
				_, err := st.DB().Exec(st.Rebind(
					`DELETE FROM attachment_pack_index WHERE blob_hash = ?`), hashB)
				require.NoError(t, err)
			},
			moves: func(hashA, hashB string) []store.RepackMove {
				return []store.RepackMove{
					{OldPackID: oldA, NewEntry: store.PackIndexEntry{BlobHash: hashA, PackID: newA, Offset: 6, StoredLen: 50, RawLen: 100}},
					{OldPackID: oldB, NewEntry: store.PackIndexEntry{BlobHash: hashB, PackID: newA, Offset: 56, StoredLen: 70, RawLen: 200}},
				}
			},
		},
		{
			name: "added current mapping",
			mutate: func(t *testing.T, st *store.Store, hashA, _ string) {
				t.Helper()
				extra := packTestHash("a813")
				fx := newPackAttachmentFixture(t, st)
				fx.addAttachment(extra, extra[:2]+"/"+extra, 300)
				_, err := st.DB().Exec(st.Rebind(`
					INSERT INTO attachment_pack_index
					    (blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
					VALUES (?, ?, 96, 40, 300, 0, 0)`), extra, oldA)
				require.NoError(t, err)
				entry, err := st.GetAttachmentPackEntry(hashA)
				require.NoError(t, err)
				require.NotNil(t, entry)
			},
			moves: func(hashA, hashB string) []store.RepackMove {
				return []store.RepackMove{
					{OldPackID: oldA, NewEntry: store.PackIndexEntry{BlobHash: hashA, PackID: newA, Offset: 6, StoredLen: 50, RawLen: 100}},
					{OldPackID: oldB, NewEntry: store.PackIndexEntry{BlobHash: hashB, PackID: newA, Offset: 56, StoredLen: 70, RawLen: 200}},
				}
			},
		},
		{
			name: "changed pack ownership",
			mutate: func(t *testing.T, st *store.Store, hashA, _ string) {
				t.Helper()
				_, err := st.DB().Exec(st.Rebind(`
					UPDATE attachment_pack_index SET pack_id = ? WHERE blob_hash = ?`), oldB, hashA)
				require.NoError(t, err)
			},
			moves: func(hashA, hashB string) []store.RepackMove {
				return []store.RepackMove{
					{OldPackID: oldA, NewEntry: store.PackIndexEntry{BlobHash: hashA, PackID: newA, Offset: 6, StoredLen: 50, RawLen: 100}},
					{OldPackID: oldB, NewEntry: store.PackIndexEntry{BlobHash: hashB, PackID: newA, Offset: 56, StoredLen: 70, RawLen: 200}},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			fx := newPackAttachmentFixture(t, st)
			hashA := packTestHash("a811")
			hashB := packTestHash("a812")
			fx.addAttachment(hashA, hashA[:2]+"/"+hashA, 100)
			fx.addAttachment(hashB, hashB[:2]+"/"+hashB, 200)
			created := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
			require.NoError(st.RecordPackedBlobs(store.PackRecord{
				PackID: oldA, EntryCount: 1, StoredBytes: 80, CreatedAt: created,
			}, []store.PackIndexEntry{{
				BlobHash: hashA, PackID: oldA, Offset: 6, StoredLen: 80, RawLen: 100,
			}}))
			require.NoError(st.RecordPackedBlobs(store.PackRecord{
				PackID: oldB, EntryCount: 1, StoredBytes: 90, CreatedAt: created,
			}, []store.PackIndexEntry{{
				BlobHash: hashB, PackID: oldB, Offset: 6, StoredLen: 90, RawLen: 200,
			}}))

			tt.mutate(t, st, hashA, hashB)
			beforeA, err := st.GetAttachmentPackEntry(hashA)
			require.NoError(err)
			beforeB, err := st.GetAttachmentPackEntry(hashB)
			require.NoError(err)

			moves := tt.moves(hashA, hashB)
			var stored int64
			for _, move := range moves {
				stored += move.NewEntry.StoredLen
			}
			err = st.CommitRepack(context.Background(), []string{oldA, oldB}, []store.PackRecord{{
				PackID: newA, EntryCount: int64(len(moves)), StoredBytes: stored,
				CreatedAt: created.Add(time.Hour),
			}}, moves)
			require.ErrorContains(err, "exact")

			afterA, getErr := st.GetAttachmentPackEntry(hashA)
			require.NoError(getErr)
			afterB, getErr := st.GetAttachmentPackEntry(hashB)
			require.NoError(getErr)
			assert.Equal(beforeA, afterA, "failed swap preserves pre-call mapping A")
			assert.Equal(beforeB, afterB, "failed swap preserves pre-call mapping B")
			has, getErr := st.HasPackRecord(newA)
			require.NoError(getErr)
			assert.False(has, "failed exact-set validation cannot publish a new record")
		})
	}
}

func TestDeleteEmptyPackRecordIsReferenceAware(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := newPackAttachmentFixture(t, st)

	const (
		livePack  = "01hzy3v7q8r9s0t1a2v3w4x6f1"
		stalePack = "01hzy3v7q8r9s0t1a2v3w4x6f2"
	)
	liveHash := packTestHash("f801")
	staleHash := packTestHash("f802")
	fx.addAttachment(liveHash, liveHash[:2]+"/"+liveHash, 100)
	created := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	require.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID: livePack, EntryCount: 1, StoredBytes: 80, CreatedAt: created,
	}, []store.PackIndexEntry{{BlobHash: liveHash, PackID: livePack, Offset: 6, StoredLen: 80, RawLen: 100}}))
	require.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID: stalePack, EntryCount: 1, StoredBytes: 90, CreatedAt: created,
	}, []store.PackIndexEntry{{BlobHash: staleHash, PackID: stalePack, Offset: 6, StoredLen: 90, RawLen: 200}}))

	deleted, err := st.DeleteEmptyPackRecord(context.Background(), livePack)
	require.NoError(err)
	assert.False(deleted)
	has, err := st.HasPackRecord(livePack)
	require.NoError(err)
	assert.True(has)

	deleted, err = st.DeleteEmptyPackRecord(context.Background(), stalePack)
	require.NoError(err)
	assert.True(deleted)
	has, err = st.HasPackRecord(stalePack)
	require.NoError(err)
	assert.False(has)
	entry, err := st.GetAttachmentPackEntry(staleHash)
	require.NoError(err)
	assert.Nil(entry, "stale index rows are deleted explicitly with the pack record")
}

func TestRepackMetadataMaintenanceHonorsCancellation(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	const packID = "01hzy3v7q8r9s0t1a2v3w4x6h1"

	_, err := st.PruneUnreferencedPackIndex(ctx)
	require.ErrorIs(err, context.Canceled)
	_, err = st.ListPackUsage(ctx)
	require.ErrorIs(err, context.Canceled)
	_, err = st.ListReferencedPackEntries(ctx, packID)
	require.ErrorIs(err, context.Canceled)
	_, err = st.DeleteEmptyPackRecord(ctx, packID)
	require.ErrorIs(err, context.Canceled)
}
