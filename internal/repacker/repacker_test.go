package repacker

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/packer"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSelectPacksEligibilityOrderingAndBudget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const (
		zeroYoung = "01hzy3v7q8r9s0t1a2v3w4x7a1"
		eligibleA = "01hzy3v7q8r9s0t1a2v3w4x7a2"
		eligibleB = "01hzy3v7q8r9s0t1a2v3w4x7a3"
		halfLive  = "01hzy3v7q8r9s0t1a2v3w4x7a4"
		tooYoung  = "01hzy3v7q8r9s0t1a2v3w4x7a5"
		tooLittle = "01hzy3v7q8r9s0t1a2v3w4x7a6"
	)
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	usage := []store.PackUsage{
		{PackRecord: store.PackRecord{PackID: eligibleA, EntryCount: 3, StoredBytes: minDeadStored + 100, CreatedAt: now.Add(-48 * time.Hour)}, LiveEntries: 1, LiveStoredBytes: 100, LiveRawBytes: 300},
		{PackRecord: store.PackRecord{PackID: halfLive, EntryCount: 2, StoredBytes: minDeadStored + 200, CreatedAt: now.Add(-48 * time.Hour)}, LiveEntries: 1, LiveStoredBytes: 100, LiveRawBytes: 100},
		{PackRecord: store.PackRecord{PackID: tooYoung, EntryCount: 3, StoredBytes: minDeadStored + 200, CreatedAt: now.Add(-24*time.Hour + time.Second)}, LiveEntries: 1, LiveStoredBytes: 100, LiveRawBytes: 100},
		{PackRecord: store.PackRecord{PackID: tooLittle, EntryCount: 3, StoredBytes: minDeadStored + 99, CreatedAt: now.Add(-48 * time.Hour)}, LiveEntries: 1, LiveStoredBytes: 100, LiveRawBytes: 100},
		{PackRecord: store.PackRecord{PackID: eligibleB, EntryCount: 3, StoredBytes: minDeadStored + 100, CreatedAt: now.Add(-25 * time.Hour)}, LiveEntries: 1, LiveStoredBytes: 100, LiveRawBytes: 100},
		{PackRecord: store.PackRecord{PackID: zeroYoung, EntryCount: 1, StoredBytes: 1, CreatedAt: now}, LiveEntries: 0},
	}

	selected, exhausted := selectPacks(usage, Options{Now: now, MaxBytes: 256})
	require.Len(selected, 3)
	assert.Equal(zeroYoung, selected[0].PackID, "zero-live packs are always retired before content rewrites")
	assert.Equal(eligibleA, selected[1].PackID)
	assert.Equal(eligibleB, selected[2].PackID, "dynamic budget selection happens after source success")
	assert.False(exhausted)

	selected, exhausted = selectPacks(usage, Options{Now: now})
	require.Len(selected, 3)
	assert.Equal([]string{zeroYoung, eligibleA, eligibleB}, packUsageIDs(selected))
	assert.False(exhausted)
}

func TestSelectPacksAcceptsExactThresholds(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	usage := store.PackUsage{
		PackRecord: store.PackRecord{
			PackID: "01hzy3v7q8r9s0t1a2v3w4x7b1", EntryCount: 3,
			StoredBytes: minDeadStored + 100, CreatedAt: now.Add(-minPackAge),
		},
		LiveEntries: 1, LiveStoredBytes: 100, LiveRawBytes: 100,
	}
	selected, exhausted := selectPacks([]store.PackUsage{usage}, Options{Now: now, MaxBytes: 100})
	assert.Equal(t, []string{usage.PackID}, packUsageIDs(selected))
	assert.False(t, exhausted)
}

func packUsageIDs(usage []store.PackUsage) []string {
	ids := make([]string, len(usage))
	for i := range usage {
		ids[i] = usage[i].PackID
	}
	return ids
}

type repackFixture struct {
	t       *testing.T
	store   *store.Store
	dir     string
	msgID   int64
	seq     int
	created time.Time
}

func newRepackFixture(t *testing.T) *repackFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err)
	convID, err := st.EnsureConversation(src.ID, "repack-thread", "Repack")
	require.NoError(t, err)
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID: convID, SourceID: src.ID, SourceMessageID: "repack-message",
		MessageType: "email", SizeEstimate: 100,
	})
	require.NoError(t, err)
	return &repackFixture{
		t: t, store: st, dir: t.TempDir(), msgID: msgID,
		created: time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC),
	}
}

func (f *repackFixture) reference(content []byte) string {
	f.t.Helper()
	f.seq++
	id := pack.ComputeBlobID(content).String()
	require.NoError(f.t, f.store.UpsertAttachment(
		f.msgID, "blob.bin", "application/octet-stream", id[:2]+"/"+id,
		id, len(content),
	))
	return id
}

func (f *repackFixture) seal(blobs ...[]byte) (store.PackRecord, []store.PackIndexEntry, string) {
	f.t.Helper()
	packsDir := filepath.Join(f.dir, "packs")
	require.NoError(f.t, os.MkdirAll(packsDir, 0o700))
	w, err := pack.NewWriter(packsDir, pack.WriterOptions{})
	require.NoError(f.t, err)
	for _, blob := range blobs {
		_, err := w.Append(blob)
		require.NoError(f.t, err)
	}
	id := w.ID()
	path := filepath.Join(packsDir, id[:2], id+blobstore.PackExt)
	entries, err := w.Seal(path)
	require.NoError(f.t, err)
	rec := store.PackRecord{PackID: id, EntryCount: int64(len(entries)), CreatedAt: f.created}
	index := make([]store.PackIndexEntry, 0, len(entries))
	for _, entry := range entries {
		rec.StoredBytes += int64(entry.StoredLen)
		index = append(index, store.PackIndexEntry{
			BlobHash: entry.ID.String(), PackID: id, Offset: int64(entry.Offset),
			StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen),
			Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
		})
	}
	require.NoError(f.t, f.store.RecordPackedBlobs(rec, index))
	return rec, index, path
}

func incompressible(t *testing.T, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	_, err := crand.Read(data)
	require.NoError(t, err)
	return data
}

func readBlob(t *testing.T, blobs *blobstore.Store, hash string) []byte {
	t.Helper()
	r, _, err := blobs.Open(hash)
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return data
}

func TestRunCombinesSparsePacksAndPreservesBytes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	liveA := []byte("live attachment A survives physical compaction")
	liveB := []byte("live attachment B survives physical compaction")
	hashA := f.reference(liveA)
	hashB := f.reference(liveB)
	dead := incompressible(t, int(minDeadStored)+(256<<10))
	oldA, _, oldPathA := f.seal(liveA, dead, []byte("second dead A"))
	oldB, _, oldPathB := f.seal(liveB, dead, []byte("second dead B"))
	blobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(blobs.Close()) }()

	stats, err := Run(context.Background(), f.store, blobs, f.dir, Options{
		TargetSize: 1 << 20, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.Equal(2, stats.PacksSelected)
	assert.Equal(2, stats.PacksRewritten)
	assert.Equal(2, stats.PacksSealed, "each sparse source commits independently")
	assert.Equal(2, stats.PacksRemoved)
	assert.Equal(2, stats.BlobsRepacked)
	assert.Equal(liveA, readBlob(t, blobs, hashA))
	assert.Equal(liveB, readBlob(t, blobs, hashB))
	for _, path := range []string{oldPathA, oldPathB} {
		_, statErr := os.Stat(path)
		require.ErrorIs(statErr, fs.ErrNotExist)
	}
	for _, oldID := range []string{oldA.PackID, oldB.PackID} {
		has, hasErr := f.store.HasPackRecord(oldID)
		require.NoError(hasErr)
		assert.False(has)
	}
	records, err := f.store.ListPackRecords()
	require.NoError(err)
	assert.Len(records, 2)
}

func TestRunAlwaysRemovesZeroLivePackWithoutWriter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	rec, _, oldPath := f.seal([]byte("unreferenced dead pack"))
	blobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(blobs.Close()) }()

	stats, err := Run(context.Background(), f.store, blobs, f.dir, Options{
		MaxBytes: 1, Now: f.created,
	})
	require.NoError(err)
	assert.Equal(1, stats.PacksSelected)
	assert.Zero(stats.PacksSealed)
	assert.Equal(1, stats.PacksRemoved)
	_, statErr := os.Stat(oldPath)
	require.ErrorIs(statErr, fs.ErrNotExist)
	has, err := f.store.HasPackRecord(rec.PackID)
	require.NoError(err)
	assert.False(has)
}

func TestRunCorruptReadLeavesOldMappingAndFileAuthoritative(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	live := []byte("corruption must abort before the mapping swap")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	rec, entries, oldPath := f.seal(live, dead, []byte("other dead"))
	var liveEntry store.PackIndexEntry
	for _, entry := range entries {
		if entry.BlobHash == hash {
			liveEntry = entry
		}
	}
	file, err := os.OpenFile(oldPath, os.O_RDWR, 0)
	require.NoError(err)
	one := []byte{0}
	_, err = file.ReadAt(one, liveEntry.Offset)
	require.NoError(err)
	one[0] ^= 0xff
	_, err = file.WriteAt(one, liveEntry.Offset)
	require.NoError(err)
	require.NoError(file.Close())
	blobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(blobs.Close()) }()

	_, err = Run(context.Background(), f.store, blobs, f.dir, Options{
		Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.Error(err)
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.Equal(rec.PackID, entry.PackID)
	_, statErr := os.Stat(oldPath)
	require.NoError(statErr)
	records, listErr := f.store.ListPackRecords()
	require.NoError(listErr)
	assert.Len(records, 1, "pre-swap failures cannot record a replacement pack")
}

func TestRunAutomaticContinuesPastCorruptSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	corruptLive := []byte("corrupt oldest source stays authoritative")
	healthyLive := []byte("healthy later source is still compacted")
	corruptHash := f.reference(corruptLive)
	healthyHash := f.reference(healthyLive)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	corrupt, corruptEntries, corruptPath := f.seal(corruptLive, dead, []byte("corrupt dead sibling"))
	healthy, _, healthyPath := f.seal(healthyLive, dead, []byte("healthy dead sibling"))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Add(-time.Hour).Format(time.RFC3339), corrupt.PackID)
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Format(time.RFC3339), healthy.PackID)
	require.NoError(err)
	for _, entry := range corruptEntries {
		if entry.BlobHash != corruptHash {
			continue
		}
		file, openErr := os.OpenFile(corruptPath, os.O_RDWR, 0)
		require.NoError(openErr)
		one := []byte{0}
		_, openErr = file.ReadAt(one, entry.Offset)
		require.NoError(openErr)
		one[0] ^= 0xff
		_, openErr = file.WriteAt(one, entry.Offset)
		require.NoError(openErr)
		require.NoError(file.Close())
		break
	}
	blobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(blobs.Close()) }()

	stats, err := Run(context.Background(), f.store, blobs, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.Error(err)
	assert.Equal(1, stats.PacksRewritten)
	corruptEntry, getErr := f.store.GetAttachmentPackEntry(corruptHash)
	require.NoError(getErr)
	require.NotNil(corruptEntry)
	assert.Equal(corrupt.PackID, corruptEntry.PackID)
	healthyEntry, getErr := f.store.GetAttachmentPackEntry(healthyHash)
	require.NoError(getErr)
	require.NotNil(healthyEntry)
	assert.NotEqual(healthy.PackID, healthyEntry.PackID)
	assert.FileExists(corruptPath)
	assert.NoFileExists(healthyPath)
}

func TestRunExplicitRetiresZeroLiveBeforeFailingOnCorruptSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	zero, _, zeroPath := f.seal([]byte("zero-live missing source needs no pack read"))
	require.NoError(os.Remove(zeroPath))
	live := []byte("explicit corrupt source fails fast")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	corrupt, entries, corruptPath := f.seal(live, dead, []byte("dead sibling"))
	healthyLive := []byte("explicit mode must not attempt a later healthy source")
	healthyHash := f.reference(healthyLive)
	healthy, _, healthyPath := f.seal(healthyLive, dead, []byte("healthy dead sibling"))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Add(-time.Hour).Format(time.RFC3339), corrupt.PackID)
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Format(time.RFC3339), healthy.PackID)
	require.NoError(err)
	for _, entry := range entries {
		if entry.BlobHash != hash {
			continue
		}
		file, err := os.OpenFile(corruptPath, os.O_RDWR, 0)
		require.NoError(err)
		one := []byte{0}
		_, err = file.ReadAt(one, entry.Offset)
		require.NoError(err)
		one[0] ^= 0xff
		_, err = file.WriteAt(one, entry.Offset)
		require.NoError(err)
		require.NoError(file.Close())
	}
	blobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(blobs.Close()) }()

	stats, err := Run(context.Background(), f.store, blobs, f.dir, Options{
		Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.Error(err)
	assert.Equal(1, stats.PacksRemoved)
	has, getErr := f.store.HasPackRecord(zero.PackID)
	require.NoError(getErr)
	assert.False(has)
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.Equal(corrupt.PackID, entry.PackID)
	assert.FileExists(corruptPath)
	healthyEntry, getErr := f.store.GetAttachmentPackEntry(healthyHash)
	require.NoError(getErr)
	require.NotNil(healthyEntry)
	assert.Equal(healthy.PackID, healthyEntry.PackID)
	assert.FileExists(healthyPath)
}

type countingBoundedStore struct {
	*blobstore.Store

	reads int
}

type injectedReadErrorStore struct {
	*blobstore.Store

	errors map[string]error
	reads  []string
}

func (s *injectedReadErrorStore) ReadBounded(hash string, maxBytes int64) ([]byte, int64, error) {
	s.reads = append(s.reads, hash)
	if err := s.errors[hash]; err != nil {
		return nil, 0, err
	}
	return s.Store.ReadBounded(hash, maxBytes)
}

func TestRunAutomaticStopsOnUnknownBoundedReadError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	brokenLive := []byte("resolver failure must stop automatic maintenance")
	healthyLive := []byte("later source must remain untouched")
	brokenHash := f.reference(brokenLive)
	healthyHash := f.reference(healthyLive)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	broken, _, brokenPath := f.seal(brokenLive, dead, []byte("broken dead sibling"))
	healthy, _, healthyPath := f.seal(healthyLive, dead, []byte("healthy dead sibling"))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Add(-time.Hour).Format(time.RFC3339), broken.PackID)
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Format(time.RFC3339), healthy.PackID)
	require.NoError(err)
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	injected := &injectedReadErrorStore{
		Store: realBlobs,
		errors: map[string]error{
			brokenHash: errors.New("injected resolver database failure"),
		},
	}

	stats, err := Run(context.Background(), f.store, injected, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.ErrorContains(err, "injected resolver database failure")
	assert.Zero(stats.PacksRewritten)
	assert.Equal([]string{brokenHash}, injected.reads)
	brokenEntry, getErr := f.store.GetAttachmentPackEntry(brokenHash)
	require.NoError(getErr)
	require.NotNil(brokenEntry)
	assert.Equal(broken.PackID, brokenEntry.PackID)
	healthyEntry, getErr := f.store.GetAttachmentPackEntry(healthyHash)
	require.NoError(getErr)
	require.NotNil(healthyEntry)
	assert.Equal(healthy.PackID, healthyEntry.PackID)
	assert.FileExists(brokenPath)
	assert.FileExists(healthyPath)
}

type injectedMaintenancePackReader struct {
	entries  []pack.Entry
	closeErr error
}

func (r *injectedMaintenancePackReader) Entries() []pack.Entry { return r.entries }
func (r *injectedMaintenancePackReader) Close() error          { return r.closeErr }

func TestRunAutomaticStopsWhenPreflightCloseFailsWithCorruption(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	brokenLive := []byte("descriptor close failure is systemic")
	healthyLive := []byte("later preflight must not run")
	brokenHash := f.reference(brokenLive)
	healthyHash := f.reference(healthyLive)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	broken, _, brokenPath := f.seal(brokenLive, dead, []byte("broken dead sibling"))
	healthy, _, healthyPath := f.seal(healthyLive, dead, []byte("healthy dead sibling"))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Add(-time.Hour).Format(time.RFC3339), broken.PackID)
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Format(time.RFC3339), healthy.PackID)
	require.NoError(err)

	originalOpen := openMaintenancePack
	var opened []string
	openMaintenancePack = func(path string) (maintenancePackReader, error) {
		opened = append(opened, path)
		if filepath.Base(path) == broken.PackID+blobstore.PackExt {
			return &injectedMaintenancePackReader{
				closeErr: errors.New("injected descriptor close failure"),
			}, nil
		}
		return originalOpen(path)
	}
	t.Cleanup(func() { openMaintenancePack = originalOpen })
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	counting := &countingBoundedStore{Store: realBlobs}

	stats, err := Run(context.Background(), f.store, counting, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.ErrorContains(err, "injected descriptor close failure")
	assert.Zero(stats.PacksRewritten)
	assert.Len(opened, 1)
	assert.Zero(counting.reads)
	for hash, oldPackID := range map[string]string{
		brokenHash:  broken.PackID,
		healthyHash: healthy.PackID,
	} {
		entry, getErr := f.store.GetAttachmentPackEntry(hash)
		require.NoError(getErr)
		require.NotNil(entry)
		assert.Equal(oldPackID, entry.PackID)
	}
	assert.FileExists(brokenPath)
	assert.FileExists(healthyPath)
}

func TestRunAutomaticStopsOnPreflightPermissionError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	brokenLive := []byte("pack stat permission failure is systemic")
	healthyLive := []byte("later source must remain untouched")
	brokenHash := f.reference(brokenLive)
	healthyHash := f.reference(healthyLive)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	broken, _, brokenPath := f.seal(brokenLive, dead, []byte("broken dead sibling"))
	healthy, _, healthyPath := f.seal(healthyLive, dead, []byte("healthy dead sibling"))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Add(-time.Hour).Format(time.RFC3339), broken.PackID)
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Format(time.RFC3339), healthy.PackID)
	require.NoError(err)

	originalOpen := openMaintenancePack
	var opened []string
	openMaintenancePack = func(path string) (maintenancePackReader, error) {
		opened = append(opened, path)
		if filepath.Base(path) == broken.PackID+blobstore.PackExt {
			return nil, &os.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
		}
		return originalOpen(path)
	}
	t.Cleanup(func() { openMaintenancePack = originalOpen })
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	counting := &countingBoundedStore{Store: realBlobs}

	stats, err := Run(context.Background(), f.store, counting, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.ErrorIs(err, fs.ErrPermission)
	assert.Zero(stats.PacksRewritten)
	assert.Len(opened, 1)
	assert.Zero(counting.reads)
	for hash, oldPackID := range map[string]string{
		brokenHash:  broken.PackID,
		healthyHash: healthy.PackID,
	} {
		entry, getErr := f.store.GetAttachmentPackEntry(hash)
		require.NoError(getErr)
		require.NotNil(entry)
		assert.Equal(oldPackID, entry.PackID)
	}
	assert.FileExists(brokenPath)
	assert.FileExists(healthyPath)
}

func TestRunAutomaticJoinsKnownContentFailuresAndCommitsHealthySource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	checksumLive := []byte("known checksum failure")
	missingLive := []byte("known missing source")
	healthyLive := []byte("healthy third source commits")
	checksumHash := f.reference(checksumLive)
	missingHash := f.reference(missingLive)
	healthyHash := f.reference(healthyLive)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	checksumPack, _, checksumPath := f.seal(checksumLive, dead, []byte("checksum dead sibling"))
	missingPack, _, missingPath := f.seal(missingLive, dead, []byte("missing dead sibling"))
	healthyPack, _, healthyPath := f.seal(healthyLive, dead, []byte("healthy dead sibling"))
	for i, packID := range []string{checksumPack.PackID, missingPack.PackID, healthyPack.PackID} {
		_, err := f.store.DB().Exec(f.store.Rebind(`
			UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
			f.created.Add(time.Duration(i-2)*time.Hour).Format(time.RFC3339), packID)
		require.NoError(err)
	}
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	injected := &injectedReadErrorStore{
		Store: realBlobs,
		errors: map[string]error{
			checksumHash: fmt.Errorf("first known checksum failure: %w", pack.ErrChecksum),
			missingHash:  fmt.Errorf("second known missing failure: %w", fs.ErrNotExist),
		},
	}

	stats, err := Run(context.Background(), f.store, injected, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.ErrorContains(err, "first known checksum failure")
	require.ErrorContains(err, "second known missing failure")
	assert.Equal(1, stats.PacksRewritten)
	assert.Equal([]string{checksumHash, missingHash, healthyHash}, injected.reads)
	for hash, oldPackID := range map[string]string{
		checksumHash: checksumPack.PackID,
		missingHash:  missingPack.PackID,
	} {
		entry, getErr := f.store.GetAttachmentPackEntry(hash)
		require.NoError(getErr)
		require.NotNil(entry)
		assert.Equal(oldPackID, entry.PackID)
	}
	healthyEntry, getErr := f.store.GetAttachmentPackEntry(healthyHash)
	require.NoError(getErr)
	require.NotNil(healthyEntry)
	assert.NotEqual(healthyPack.PackID, healthyEntry.PackID)
	assert.FileExists(checksumPath)
	assert.FileExists(missingPath)
	assert.NoFileExists(healthyPath)
}

func TestRunAutomaticStopsBeforeTouchingSourcesPastCommittedBudget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	firstLive := []byte("first healthy source crosses the small budget")
	missingLive := []byte("missing later source must not be inspected")
	laterLive := []byte("later healthy source must also remain untouched")
	firstHash := f.reference(firstLive)
	missingHash := f.reference(missingLive)
	laterHash := f.reference(laterLive)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	first, _, firstPath := f.seal(firstLive, dead, []byte("first dead sibling"))
	missing, _, missingPath := f.seal(missingLive, dead, []byte("missing dead sibling"))
	later, _, laterPath := f.seal(laterLive, dead, []byte("later dead sibling"))
	for i, packID := range []string{first.PackID, missing.PackID, later.PackID} {
		_, err := f.store.DB().Exec(f.store.Rebind(`
			UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
			f.created.Add(time.Duration(i-2)*time.Hour).Format(time.RFC3339), packID)
		require.NoError(err)
	}
	require.NoError(os.Remove(missingPath))

	originalOpen := openMaintenancePack
	var opened []string
	openMaintenancePack = func(path string) (maintenancePackReader, error) {
		opened = append(opened, path)
		return originalOpen(path)
	}
	t.Cleanup(func() { openMaintenancePack = originalOpen })
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	injected := &injectedReadErrorStore{Store: realBlobs, errors: map[string]error{}}

	stats, err := Run(context.Background(), f.store, injected, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.True(stats.BudgetExhausted)
	assert.Equal(1, stats.PacksRewritten)
	assert.Equal(1, stats.PacksSealed)
	assert.Equal(1, stats.PacksRemoved)
	assert.Equal(1, stats.BlobsRepacked)
	assert.Equal(int64(len(firstLive)), stats.BytesRepacked)
	assert.Zero(stats.PacksDeferredOversized)
	require.Len(opened, 1)
	assert.Equal(first.PackID+blobstore.PackExt, filepath.Base(opened[0]))
	assert.Equal([]string{firstHash}, injected.reads)

	firstEntry, getErr := f.store.GetAttachmentPackEntry(firstHash)
	require.NoError(getErr)
	require.NotNil(firstEntry)
	assert.NotEqual(first.PackID, firstEntry.PackID)
	assert.NoFileExists(firstPath)
	for hash, oldPackID := range map[string]string{
		missingHash: missing.PackID,
		laterHash:   later.PackID,
	} {
		entry, entryErr := f.store.GetAttachmentPackEntry(hash)
		require.NoError(entryErr)
		require.NotNil(entry)
		assert.Equal(oldPackID, entry.PackID)
		has, hasErr := f.store.HasPackRecord(oldPackID)
		require.NoError(hasErr)
		assert.True(has)
	}
	assert.NoFileExists(missingPath)
	assert.FileExists(laterPath)
}

func TestRunAutomaticWriterCreationFailureStopsBeforeLaterSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	brokenLive := []byte("writer creation failure is systemic")
	healthyLive := []byte("later source must remain untouched")
	brokenHash := f.reference(brokenLive)
	healthyHash := f.reference(healthyLive)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	broken, _, brokenPath := f.seal(brokenLive, dead, []byte("broken dead sibling"))
	healthy, _, healthyPath := f.seal(healthyLive, dead, []byte("healthy dead sibling"))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Add(-time.Hour).Format(time.RFC3339), broken.PackID)
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		f.created.Format(time.RFC3339), healthy.PackID)
	require.NoError(err)
	originalFactory := newPackWriter
	newPackWriter = func(string, pack.WriterOptions) (packWriter, error) {
		return nil, errors.New("injected automatic writer creation failure")
	}
	t.Cleanup(func() { newPackWriter = originalFactory })
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	counting := &countingBoundedStore{Store: realBlobs}

	stats, err := Run(context.Background(), f.store, counting, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.ErrorContains(err, "injected automatic writer creation failure")
	assert.Zero(stats.PacksRewritten)
	assert.Equal(1, counting.reads)
	for hash, oldPackID := range map[string]string{
		brokenHash:  broken.PackID,
		healthyHash: healthy.PackID,
	} {
		entry, getErr := f.store.GetAttachmentPackEntry(hash)
		require.NoError(getErr)
		require.NotNil(entry)
		assert.Equal(oldPackID, entry.PackID)
	}
	assert.FileExists(brokenPath)
	assert.FileExists(healthyPath)
}

func (s *countingBoundedStore) ReadBounded(hash string, maxBytes int64) ([]byte, int64, error) {
	s.reads++
	return s.Store.ReadBounded(hash, maxBytes)
}

func TestRunDefersOversizedLiveUsageBeforeBudgetAndRead(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	live := []byte("forged accounting must not trigger an oversized allocation")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	old, _, oldPath := f.seal(live, dead, []byte("dead sibling"))
	oversized := int64(blobstore.MaxMaintenanceBlobBytes + 1)
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_pack_index SET raw_len = ? WHERE blob_hash = ?`), oversized, hash)
	require.NoError(err)
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	counting := &countingBoundedStore{Store: realBlobs}

	stats, err := Run(context.Background(), f.store, counting, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.Equal(1, stats.PacksDeferredOversized)
	assert.Zero(stats.BytesRepacked)
	assert.Zero(counting.reads)
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.Equal(old.PackID, entry.PackID)
	assert.FileExists(oldPath)
}

func TestRunDefersOversizedStoredUsageBeforeBudgetAndRead(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	live := []byte("forged stored accounting must not trigger an oversized allocation")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	old, _, oldPath := f.seal(live, dead, []byte("dead sibling"))
	oversized := int64(blobstore.MaxMaintenanceBlobBytes + 1)
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_pack_index SET stored_len = ? WHERE blob_hash = ?`), oversized, hash)
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_packs SET stored_bytes = ? WHERE pack_id = ?`),
		oversized+minDeadStored, old.PackID)
	require.NoError(err)
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	counting := &countingBoundedStore{Store: realBlobs}

	stats, err := Run(context.Background(), f.store, counting, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.Equal(1, stats.PacksDeferredOversized)
	assert.Zero(stats.BytesRepacked)
	assert.Zero(counting.reads)
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.Equal(old.PackID, entry.PackID)
	assert.FileExists(oldPath)
}

func TestRunRejectsFooterIndexMismatchBeforeContentRead(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	live := []byte("footer metadata remains authoritative")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	old, _, oldPath := f.seal(live, dead, []byte("dead sibling"))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachment_pack_index SET pack_offset = pack_offset + 1 WHERE blob_hash = ?`), hash)
	require.NoError(err)
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	counting := &countingBoundedStore{Store: realBlobs}

	stats, err := Run(context.Background(), f.store, counting, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.ErrorContains(err, "does not match pack footer")
	assert.Zero(stats.PacksRewritten)
	assert.Zero(counting.reads)
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.Equal(old.PackID, entry.PackID)
	assert.FileExists(oldPath)
}

func TestRunDefersOversizedContainerWithoutContentRead(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	live := []byte("oversized container defers intact")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	old, _, oldPath := f.seal(live, dead, []byte("dead sibling"))
	require.NoError(os.Truncate(oldPath, blobstore.MaxMaintenancePackBytes+1))
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	counting := &countingBoundedStore{Store: realBlobs}

	stats, err := Run(context.Background(), f.store, counting, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.Equal(1, stats.PacksDeferredOversized)
	assert.Zero(counting.reads)
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.Equal(old.PackID, entry.PackID)
	assert.FileExists(oldPath)
}

func TestRunDefersOversizedFooterAndEntryCountWithoutContentRead(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "footer",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				file, err := os.OpenFile(path, os.O_RDWR, 0)
				require.NoError(t, err)
				info, err := file.Stat()
				require.NoError(t, err)
				var encoded [4]byte
				binary.LittleEndian.PutUint32(encoded[:], blobstore.MaxMaintenanceFooterBytes+1)
				_, err = file.WriteAt(encoded[:], info.Size()-40)
				require.NoError(t, err)
				require.NoError(t, file.Close())
			},
		},
		{
			name: "entry count",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				footerLen := int(binary.LittleEndian.Uint32(data[len(data)-40:]))
				footerStart := len(data) - 40 - footerLen
				binary.LittleEndian.PutUint32(data[footerStart:], blobstore.MaxMaintenancePackEntries+1)
				require.NoError(t, os.WriteFile(path, data, 0o600))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newRepackFixture(t)
			live := []byte("bounded pack metadata remains authoritative")
			hash := f.reference(live)
			dead := incompressible(t, int(minDeadStored)+(128<<10))
			old, _, oldPath := f.seal(live, dead, []byte("dead sibling"))
			tt.mutate(t, oldPath)
			realBlobs := blobstore.New(f.store, f.dir)
			defer func() { require.NoError(realBlobs.Close()) }()
			counting := &countingBoundedStore{Store: realBlobs}

			stats, err := Run(context.Background(), f.store, counting, f.dir, Options{
				MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
			})
			require.NoError(err)
			assert.Equal(1, stats.PacksDeferredOversized)
			assert.Zero(counting.reads)
			entry, getErr := f.store.GetAttachmentPackEntry(hash)
			require.NoError(getErr)
			require.NotNil(entry)
			assert.Equal(old.PackID, entry.PackID)
			assert.FileExists(oldPath)
		})
	}
}

func rewriteFooterRawLen(t *testing.T, path, hash string, rawLen uint64) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(data), 40)
	trailer := data[len(data)-40:]
	footerLen := int(binary.LittleEndian.Uint32(trailer[:4]))
	footer := data[len(data)-40-footerLen : len(data)-40]
	count := int(binary.LittleEndian.Uint32(footer[:4]))
	wanted, err := pack.ParseBlobID(hash)
	require.NoError(t, err)
	found := false
	for i := range count {
		off := 4 + i*61
		if bytes.Equal(footer[off:off+32], wanted[:]) {
			binary.LittleEndian.PutUint64(footer[off+48:], rawLen)
			found = true
			break
		}
	}
	require.True(t, found)
	digest := sha256.New()
	_, err = digest.Write(footer)
	require.NoError(t, err)
	_, err = digest.Write(trailer[:4])
	require.NoError(t, err)
	copy(trailer[4:36], digest.Sum(nil))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func TestRunIgnoresOversizedDeadFooterEntry(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	live := []byte("small live entry remains eligible")
	deadHugeRaw := bytes.Repeat([]byte("z"), 1024)
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	old, _, oldPath := f.seal(live, deadHugeRaw, dead, []byte("dead sibling"))
	deadHash := pack.ComputeBlobID(deadHugeRaw).String()
	rewriteFooterRawLen(t, oldPath, deadHash, blobstore.MaxMaintenanceBlobBytes+1)
	blobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(blobs.Close()) }()

	stats, err := Run(context.Background(), f.store, blobs, f.dir, Options{
		Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.Zero(stats.PacksDeferredOversized)
	assert.Equal(1, stats.PacksRewritten)
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.NotEqual(old.PackID, entry.PackID)
	assert.NoFileExists(oldPath)
}

func TestRunSealsBeforeReplacementEntryLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	liveA := []byte("first live entry")
	liveB := []byte("second live entry")
	f.reference(liveA)
	f.reference(liveB)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	f.seal(liveA, liveB, dead, []byte("dead sibling one"), []byte("dead sibling two"))
	oldLimit := maxReplacementPackEntries
	maxReplacementPackEntries = 1
	t.Cleanup(func() { maxReplacementPackEntries = oldLimit })
	blobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(blobs.Close()) }()

	stats, err := Run(context.Background(), f.store, blobs, f.dir, Options{
		Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.Equal(2, stats.PacksSealed)
	records, err := f.store.ListPackRecords()
	require.NoError(err)
	require.Len(records, 2)
	for _, record := range records {
		assert.Equal(int64(1), record.EntryCount)
	}
}

func TestRunDoesNotReportSealedAttemptAsCommittedProgress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	liveA := []byte("first live entry seals before later failure")
	liveB := []byte("second live entry is corrupt")
	hashA := f.reference(liveA)
	hashB := f.reference(liveB)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	old, _, oldPath := f.seal(liveA, liveB, dead, []byte("dead sibling one"), []byte("dead sibling two"))
	referenced, err := f.store.ListReferencedPackEntries(context.Background(), old.PackID)
	require.NoError(err)
	require.Len(referenced, 2)
	for _, entry := range referenced[1:] {
		file, err := os.OpenFile(oldPath, os.O_RDWR, 0)
		require.NoError(err)
		one := []byte{0}
		_, err = file.ReadAt(one, entry.Offset)
		require.NoError(err)
		one[0] ^= 0xff
		_, err = file.WriteAt(one, entry.Offset)
		require.NoError(err)
		require.NoError(file.Close())
	}
	originalFactory := newPackWriter
	newPackWriter = func(dir string, opts pack.WriterOptions) (packWriter, error) {
		writer, err := pack.NewWriter(dir, opts)
		if err != nil {
			return nil, fmt.Errorf("create always-full test writer: %w", err)
		}
		return alwaysFullWriter{packWriter: writer}, nil
	}
	t.Cleanup(func() { newPackWriter = originalFactory })
	blobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(blobs.Close()) }()

	stats, err := Run(context.Background(), f.store, blobs, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.Error(err)
	assert.Zero(stats.PacksRewritten)
	assert.Zero(stats.PacksSealed)
	assert.Zero(stats.BlobsRepacked)
	assert.Zero(stats.BytesRepacked)
	for _, hash := range []string{hashA, hashB} {
		entry, getErr := f.store.GetAttachmentPackEntry(hash)
		require.NoError(getErr)
		require.NotNil(entry)
		assert.Equal(old.PackID, entry.PackID)
	}
	var packFiles []string
	err = filepath.WalkDir(filepath.Join(f.dir, "packs"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && filepath.Ext(path) == blobstore.PackExt {
			packFiles = append(packFiles, path)
		}
		return nil
	})
	require.NoError(err)
	assert.Len(packFiles, 2, "the sealed replacement remains an unreported reconciliation-safe orphan")
}

func TestRunJoinsZeroLiveCleanupAndAutomaticContentErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	zero, _, zeroPath := f.seal([]byte("held zero-live source"))
	live := []byte("corrupt source after cleanup failure")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	corrupt, entries, corruptPath := f.seal(live, dead, []byte("dead sibling"))
	for _, entry := range entries {
		if entry.BlobHash != hash {
			continue
		}
		file, err := os.OpenFile(corruptPath, os.O_RDWR, 0)
		require.NoError(err)
		one := []byte{0}
		_, err = file.ReadAt(one, entry.Offset)
		require.NoError(err)
		one[0] ^= 0xff
		_, err = file.WriteAt(one, entry.Offset)
		require.NoError(err)
		require.NoError(file.Close())
	}
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	failing := &selectiveRetireStore{
		Store: realBlobs,
		failures: map[string]error{
			zero.PackID: errors.New("injected zero-live retirement failure"),
		},
	}

	stats, err := Run(context.Background(), f.store, failing, f.dir, Options{
		MaxBytes: 1, Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	require.ErrorContains(err, "injected zero-live retirement failure")
	require.ErrorContains(err, "read live blob")
	assert.Zero(stats.PacksRewritten)
	assert.FileExists(zeroPath)
	has, getErr := f.store.HasPackRecord(zero.PackID)
	require.NoError(getErr)
	assert.True(has)
	has, getErr = f.store.HasPackRecord(corrupt.PackID)
	require.NoError(getErr)
	assert.True(has)
}

type failFirstRetireStore struct {
	*blobstore.Store

	failed bool
}

func (s *failFirstRetireStore) RetirePack(packID string) error {
	if !s.failed {
		s.failed = true
		return errors.New("injected external reader hold")
	}
	return s.Store.RetirePack(packID)
}

func TestRunRetirementFailureRetainsRetryableZeroLiveRecord(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	live := []byte("mapping moves before retryable physical retirement")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	old, _, oldPath := f.seal(live, dead, []byte("other dead"))
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	failing := &failFirstRetireStore{Store: realBlobs}
	opts := Options{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)}

	_, err := Run(context.Background(), f.store, failing, f.dir, opts)
	require.ErrorContains(err, "injected external reader hold")
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.NotEqual(old.PackID, entry.PackID, "database swap remains committed")
	has, hasErr := f.store.HasPackRecord(old.PackID)
	require.NoError(hasErr)
	assert.True(has, "old zero-live inventory remains truthful while deletion failed")
	_, statErr := os.Stat(oldPath)
	require.NoError(statErr)

	stats, err := Run(context.Background(), f.store, realBlobs, f.dir, opts)
	require.NoError(err)
	assert.Equal(1, stats.PacksRemoved)
	has, hasErr = f.store.HasPackRecord(old.PackID)
	require.NoError(hasErr)
	assert.False(has)
	_, statErr = os.Stat(oldPath)
	assert.ErrorIs(statErr, fs.ErrNotExist)
}

type selectiveRetireStore struct {
	*blobstore.Store

	failures map[string]error
	calls    []string
}

func (s *selectiveRetireStore) RetirePack(packID string) error {
	s.calls = append(s.calls, packID)
	if err := s.failures[packID]; err != nil {
		return err
	}
	return s.Store.RetirePack(packID)
}

func TestRunCleanupContinuesAndJoinsRetirementErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	failedA, _, failedPathA := f.seal([]byte("first externally held zero-live pack"))
	failedB, _, failedPathB := f.seal([]byte("second externally held zero-live pack"))
	removed, _, removedPath := f.seal([]byte("independent zero-live pack can still be reclaimed"))
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	retirer := &selectiveRetireStore{
		Store: realBlobs,
		failures: map[string]error{
			failedA.PackID: errors.New("first injected external reader hold"),
			failedB.PackID: errors.New("second injected external reader hold"),
		},
	}

	stats, err := Run(context.Background(), f.store, retirer, f.dir, Options{})
	require.Error(err)
	require.ErrorContains(err, "first injected external reader hold")
	require.ErrorContains(err, "second injected external reader hold")
	assert.Len(retirer.calls, 3, "one retirement failure must not stop later cleanup")
	assert.Equal(1, stats.PacksRemoved)

	for _, retained := range []struct {
		record store.PackRecord
		path   string
	}{{failedA, failedPathA}, {failedB, failedPathB}} {
		has, hasErr := f.store.HasPackRecord(retained.record.PackID)
		require.NoError(hasErr)
		assert.True(has, "failed retirement retains truthful inventory")
		assert.FileExists(retained.path)
	}
	has, hasErr := f.store.HasPackRecord(removed.PackID)
	require.NoError(hasErr)
	assert.False(has)
	assert.NoFileExists(removedPath)
}

func TestRunCanceledBeforeSelectionLeavesStateUntouched(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	rec, _, oldPath := f.seal([]byte("dead but cancellation wins before selection"))
	blobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(blobs.Close()) }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Run(ctx, f.store, blobs, f.dir, Options{})
	require.ErrorIs(err, context.Canceled)
	has, hasErr := f.store.HasPackRecord(rec.PackID)
	require.NoError(hasErr)
	assert.True(has)
	_, statErr := os.Stat(oldPath)
	assert.NoError(statErr)
}

type cancelBeforeSealWriter struct {
	packWriter

	cancel context.CancelFunc
}

type alwaysFullWriter struct{ packWriter }

func (alwaysFullWriter) Full() bool { return true }

func (w cancelBeforeSealWriter) Full() bool {
	w.cancel()
	return true
}

func TestRunCancellationAtSealBoundaryAbortsStaging(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	live := []byte("cancellation immediately before seal keeps old authority")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	old, _, oldPath := f.seal(live, dead, []byte("other dead"))
	blobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(blobs.Close()) }()
	ctx, cancel := context.WithCancel(context.Background())

	originalFactory := newPackWriter
	newPackWriter = func(dir string, opts pack.WriterOptions) (packWriter, error) {
		writer, err := pack.NewWriter(dir, opts)
		if err != nil {
			return nil, fmt.Errorf("create cancel-before-seal writer: %w", err)
		}
		return cancelBeforeSealWriter{packWriter: writer, cancel: cancel}, nil
	}
	t.Cleanup(func() { newPackWriter = originalFactory })

	stats, err := Run(ctx, f.store, blobs, f.dir,
		Options{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)})
	require.ErrorIs(err, context.Canceled)
	assert.Zero(stats.PacksSealed)
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.Equal(old.PackID, entry.PackID)
	assert.FileExists(oldPath)
	staging, globErr := filepath.Glob(filepath.Join(f.dir, "packs", "*.staging"))
	require.NoError(globErr)
	assert.Empty(staging, "seal-boundary cancellation aborts the active writer")
}

type cancelAfterFirstRetireStore struct {
	*blobstore.Store

	cancel  context.CancelFunc
	retired string
}

func (s *cancelAfterFirstRetireStore) RetirePack(packID string) error {
	err := s.Store.RetirePack(packID)
	if s.retired == "" {
		s.retired = packID
		s.cancel()
	}
	return err
}

func TestRunCancellationAtCleanupBoundaryLeavesRetryableInventory(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	recordA, _, pathA := f.seal([]byte("cleanup cancellation zero-live A"))
	recordB, _, pathB := f.seal([]byte("cleanup cancellation zero-live B"))
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	ctx, cancel := context.WithCancel(context.Background())
	canceling := &cancelAfterFirstRetireStore{Store: realBlobs, cancel: cancel}

	stats, err := Run(ctx, f.store, canceling, f.dir, Options{})
	require.ErrorIs(err, context.Canceled)
	assert.Zero(stats.PacksRemoved)
	require.NotEmpty(canceling.retired)

	for _, candidate := range []struct {
		record store.PackRecord
		path   string
	}{{recordA, pathA}, {recordB, pathB}} {
		has, hasErr := f.store.HasPackRecord(candidate.record.PackID)
		require.NoError(hasErr)
		if candidate.record.PackID == canceling.retired {
			assert.True(has, "canceled record cleanup retains inventory for dangling-record repair")
			assert.NoFileExists(candidate.path)
			continue
		}
		assert.True(has, "unattempted cleanup remains truthful and retryable")
		assert.FileExists(candidate.path)
	}

	retryStats, retryErr := Run(context.Background(), f.store, realBlobs, f.dir, Options{})
	require.NoError(retryErr)
	assert.Equal(2, retryStats.PacksRemoved)
}

type cancelAfterReadStore struct {
	*blobstore.Store

	cancel context.CancelFunc
}

func (s cancelAfterReadStore) ReadBounded(hash string, maxBytes int64) ([]byte, int64, error) {
	data, size, err := s.Store.ReadBounded(hash, maxBytes)
	if err == nil {
		s.cancel()
	}
	return data, size, err
}

func TestRunCancellationAfterVerifiedReadAbortsActiveWriter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	live := []byte("cancellation after read must not publish a replacement")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	old, _, oldPath := f.seal(live, dead, []byte("other dead"))
	realBlobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(realBlobs.Close()) }()
	ctx, cancel := context.WithCancel(context.Background())
	canceling := cancelAfterReadStore{Store: realBlobs, cancel: cancel}

	stats, err := Run(ctx, f.store, canceling, f.dir,
		Options{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)})
	require.ErrorIs(err, context.Canceled)
	assert.Zero(stats.PacksSealed, "cancellation at the read boundary aborts staging before seal")
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.Equal(old.PackID, entry.PackID)
	assert.FileExists(oldPath)
}

type mismatchedOpenStore struct {
	data []byte
}

func (s mismatchedOpenStore) ReadBounded(string, int64) ([]byte, int64, error) {
	return s.data, int64(len(s.data)), nil
}

func (mismatchedOpenStore) RetirePack(string) error { return nil }

func TestRunRequiresAppendedBlobIDToMatchExpectedHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	live := []byte("expected live bytes")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	old, _, oldPath := f.seal(live, dead, []byte("other dead"))

	_, err := Run(context.Background(), f.store,
		mismatchedOpenStore{data: bytes.Repeat([]byte{'x'}, len(live))}, f.dir,
		Options{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)})
	require.ErrorContains(err, "does not match expected")
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.Equal(old.PackID, entry.PackID)
	_, statErr := os.Stat(oldPath)
	assert.NoError(statErr)
}

type appendFailWriter struct{ packWriter }

func (w appendFailWriter) Append([]byte) (pack.Entry, error) {
	return pack.Entry{}, errors.New("injected append failure")
}

type sealFailWriter struct{ packWriter }

func (w sealFailWriter) Seal(string) ([]pack.Entry, error) {
	return nil, errors.New("injected seal failure")
}

func TestRunWriterFailuresAbortStagingAndPreserveOldAuthority(t *testing.T) {
	tests := []struct {
		name string
		wrap func(packWriter) packWriter
		want string
	}{
		{name: "append", wrap: func(w packWriter) packWriter { return appendFailWriter{w} }, want: "injected append failure"},
		{name: "seal", wrap: func(w packWriter) packWriter { return sealFailWriter{w} }, want: "injected seal failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newRepackFixture(t)
			live := []byte("writer failure leaves old packed bytes authoritative")
			hash := f.reference(live)
			dead := incompressible(t, int(minDeadStored)+(128<<10))
			old, _, oldPath := f.seal(live, dead, []byte("other dead"))
			blobs := blobstore.New(f.store, f.dir)
			defer func() { require.NoError(blobs.Close()) }()

			originalFactory := newPackWriter
			newPackWriter = func(dir string, opts pack.WriterOptions) (packWriter, error) {
				writer, err := pack.NewWriter(dir, opts)
				if err != nil {
					return nil, fmt.Errorf("create injected writer: %w", err)
				}
				return tt.wrap(writer), nil
			}
			t.Cleanup(func() { newPackWriter = originalFactory })

			_, err := Run(context.Background(), f.store, blobs, f.dir,
				Options{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)})
			require.ErrorContains(err, tt.want)
			entry, getErr := f.store.GetAttachmentPackEntry(hash)
			require.NoError(getErr)
			require.NotNil(entry)
			assert.Equal(old.PackID, entry.PackID)
			assert.FileExists(oldPath)
			staging, globErr := filepath.Glob(filepath.Join(f.dir, "packs", "*.staging"))
			require.NoError(globErr)
			assert.Empty(staging, "failed active writer is aborted")
		})
	}
}

func TestRunCASFailureLeavesOldMappingsAndSealedOrphan(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite trigger injects a CAS failure")
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	live := []byte("CAS failure retains old mapping and pack")
	hash := f.reference(live)
	dead := incompressible(t, int(minDeadStored)+(128<<10))
	old, _, oldPath := f.seal(live, dead, []byte("other dead"))
	_, err := f.store.DB().Exec(`
		CREATE TRIGGER fail_repack_cas
		BEFORE UPDATE OF pack_id ON attachment_pack_index
		BEGIN SELECT RAISE(ABORT, 'injected repack CAS failure'); END`)
	require.NoError(err)
	blobs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(blobs.Close()) }()

	_, err = Run(context.Background(), f.store, blobs, f.dir,
		Options{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)})
	require.ErrorContains(err, "injected repack CAS failure")
	entry, getErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(getErr)
	require.NotNil(entry)
	assert.Equal(old.PackID, entry.PackID)
	assert.FileExists(oldPath)
	records, listErr := f.store.ListPackRecords()
	require.NoError(listErr)
	assert.Len(records, 1, "new sealed pack is not recorded when swap rolls back")
	var packFiles []string
	walkErr := filepath.WalkDir(filepath.Join(f.dir, "packs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == blobstore.PackExt {
			packFiles = append(packFiles, path)
		}
		return nil
	})
	require.NoError(walkErr)
	assert.Len(packFiles, 2, "sealed unrecorded replacement remains for reconciliation")
}

func TestRunRecordCleanupFailureIsRepairedAsDanglingInventory(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite trigger injects post-delete record cleanup failure")
	require := require.New(t)
	assert := assert.New(t)
	f := newRepackFixture(t)
	old, _, oldPath := f.seal([]byte("zero-live pack for record-cleanup crash boundary"))
	_, err := f.store.DB().Exec(`
		CREATE TRIGGER fail_pack_record_cleanup
		BEFORE DELETE ON attachment_packs
		BEGIN SELECT RAISE(ABORT, 'injected record cleanup failure'); END`)
	require.NoError(err)
	blobs := blobstore.New(f.store, f.dir)

	_, err = Run(context.Background(), f.store, blobs, f.dir, Options{})
	require.ErrorContains(err, "injected record cleanup failure")
	assert.NoFileExists(oldPath, "physical retirement committed before record cleanup")
	has, hasErr := f.store.HasPackRecord(old.PackID)
	require.NoError(hasErr)
	assert.True(has, "missing-file record remains truthful and retryable")
	require.NoError(blobs.Close())
	_, err = f.store.DB().Exec(`DROP TRIGGER fail_pack_record_cleanup`)
	require.NoError(err)

	packStats, err := packer.Run(context.Background(), f.store, f.dir, packer.Options{})
	require.NoError(err)
	assert.Equal(1, packStats.RecordsDropped)
	has, hasErr = f.store.HasPackRecord(old.PackID)
	require.NoError(hasErr)
	assert.False(has)
}

func writeLifecycleBlob(t *testing.T, dir string, content []byte) string {
	t.Helper()
	hash := pack.ComputeBlobID(content).String()
	path := filepath.Join(dir, hash[:2], hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return hash
}

func addLifecycleMessage(t *testing.T, st *store.Store, source *store.Source, key string) int64 {
	t.Helper()
	convID, err := st.EnsureConversation(source.ID, "thread-"+key, "Lifecycle")
	require.NoError(t, err)
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID: convID, SourceID: source.ID,
		SourceMessageID: "message-" + key, MessageType: "email",
	})
	require.NoError(t, err)
	return msgID
}

func addLifecycleAttachment(
	t *testing.T,
	st *store.Store,
	msgID int64,
	filename, contentHash, thumbnailHash string,
	contentSize int,
) {
	t.Helper()
	require.NoError(t, st.UpsertAttachment(msgID, filename, "application/octet-stream",
		contentHash[:2]+"/"+contentHash, contentHash, contentSize))
	if thumbnailHash == "" {
		return
	}
	_, err := st.DB().Exec(st.Rebind(`
		UPDATE attachments SET thumbnail_hash = ?, thumbnail_path = ?
		WHERE message_id = ? AND filename = ?`),
		thumbnailHash, thumbnailHash[:2]+"/"+thumbnailHash, msgID, filename)
	require.NoError(t, err)
}

func TestPackedAttachmentLifecycleRemoveRepackUnpackAndUpgrade(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	sourceA, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	sourceB, err := st.GetOrCreateSource("gmail", "bob@example.com")
	require.NoError(err)
	msgA := addLifecycleMessage(t, st, sourceA, "a")
	msgB := addLifecycleMessage(t, st, sourceB, "b")

	sharedContent := []byte("shared content survives source removal")
	sharedThumb := []byte("shared thumbnail survives cross-column references")
	uniqueA := []byte("source A unique content is logically deleted")
	uniqueAThumb := []byte("source A unique thumbnail is logically deleted")
	uniqueB := []byte("source B unique content survives")
	uniqueBThumb := []byte("source B unique thumbnail survives")
	contents := map[string][]byte{}
	for _, content := range [][]byte{
		sharedContent, sharedThumb, uniqueA, uniqueAThumb, uniqueB, uniqueBThumb,
	} {
		contents[writeLifecycleBlob(t, dir, content)] = content
	}
	sharedHash := pack.ComputeBlobID(sharedContent).String()
	sharedThumbHash := pack.ComputeBlobID(sharedThumb).String()
	uniqueAHash := pack.ComputeBlobID(uniqueA).String()
	uniqueAThumbHash := pack.ComputeBlobID(uniqueAThumb).String()
	uniqueBHash := pack.ComputeBlobID(uniqueB).String()
	uniqueBThumbHash := pack.ComputeBlobID(uniqueBThumb).String()

	addLifecycleAttachment(t, st, msgA, "a-shared.bin", sharedHash, uniqueAThumbHash, len(sharedContent))
	addLifecycleAttachment(t, st, msgA, "a-unique.bin", uniqueAHash, sharedThumbHash, len(uniqueA))
	addLifecycleAttachment(t, st, msgB, "b-shared.bin", sharedHash, uniqueBThumbHash, len(sharedContent))
	addLifecycleAttachment(t, st, msgB, "b-unique.bin", uniqueBHash, sharedThumbHash, len(uniqueB))

	// Source A owns enough additional incompressible unique content to make
	// the old pack both sparse and above the physical-GC dead-byte threshold.
	deadHashes := []string{uniqueAHash, uniqueAThumbHash}
	for i := range 6 {
		size := 64 + i
		if i == 0 {
			size = int(minDeadStored) + (256 << 10)
		}
		content := incompressible(t, size)
		hash := writeLifecycleBlob(t, dir, content)
		contents[hash] = content
		deadHashes = append(deadHashes, hash)
		addLifecycleAttachment(t, st, msgA, "a-dead-"+string(rune('a'+i))+".bin", hash, "", len(content))
	}

	packStats, err := packer.Run(context.Background(), st, dir, packer.Options{})
	require.NoError(err)
	assert.Equal(len(contents), packStats.BlobsPacked)
	records, err := st.ListPackRecords()
	require.NoError(err)
	require.Len(records, 1)
	oldPackID := records[0].PackID
	oldPath := filepath.Join(dir, "packs", oldPackID[:2], oldPackID+blobstore.PackExt)
	_, err = st.DB().Exec(st.Rebind(`UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339), oldPackID)
	require.NoError(err)

	_, mappingsRemoved, err := st.RemoveSourceSerialized(context.Background(), sourceA.ID)
	require.NoError(err)
	assert.Equal(int64(len(deadHashes)), mappingsRemoved)
	blobs := blobstore.New(st, dir)
	for _, hash := range deadHashes {
		_, _, openErr := blobs.Open(hash)
		require.ErrorIs(openErr, fs.ErrNotExist, "deleted hash %s cannot be resurrected", hash)
	}
	for hash, want := range map[string][]byte{
		sharedHash: sharedContent, sharedThumbHash: sharedThumb,
		uniqueBHash: uniqueB, uniqueBThumbHash: uniqueBThumb,
	} {
		assert.Equal(want, readBlob(t, blobs, hash))
	}

	repackStats, err := Run(context.Background(), st, blobs, dir, Options{})
	require.NoError(err)
	assert.Equal(1, repackStats.PacksRewritten)
	assert.Equal(1, repackStats.PacksRemoved)
	assert.NoFileExists(oldPath)
	for hash, want := range map[string][]byte{
		sharedHash: sharedContent, sharedThumbHash: sharedThumb,
		uniqueBHash: uniqueB, uniqueBThumbHash: uniqueBThumb,
	} {
		assert.Equal(want, readBlob(t, blobs, hash))
	}
	require.NoError(blobs.Close(), "close daemon cache before host-local unpack")

	unpackStats, err := packer.Unpack(context.Background(), st, dir)
	require.NoError(err)
	assert.Equal(4, unpackStats.BlobsRestored)
	records, err = st.ListPackRecords()
	require.NoError(err)
	assert.Empty(records)
	indexed, err := st.ListIndexedBlobHashes()
	require.NoError(err)
	assert.Empty(indexed)
	for hash, want := range map[string][]byte{
		sharedHash: sharedContent, sharedThumbHash: sharedThumb,
		uniqueBHash: uniqueB, uniqueBThumbHash: uniqueBThumb,
	} {
		got, readErr := os.ReadFile(filepath.Join(dir, hash[:2], hash))
		require.NoError(readErr)
		assert.Equal(want, got)
	}
	for _, hash := range deadHashes {
		assert.NoFileExists(filepath.Join(dir, hash[:2], hash), "dead hash must not reappear during unpack")
	}

	upgradeStats, err := packer.Run(context.Background(), st, dir, packer.Options{})
	require.NoError(err)
	assert.Equal(4, upgradeStats.BlobsPacked)
	upgraded := blobstore.New(st, dir)
	defer func() { require.NoError(upgraded.Close()) }()
	assert.Equal(sharedContent, readBlob(t, upgraded, sharedHash))
	noopStats, err := Run(context.Background(), st, upgraded, dir, Options{})
	require.NoError(err)
	assert.Zero(noopStats.PacksSelected, "fully live upgraded packs need no physical rewrite")
}
