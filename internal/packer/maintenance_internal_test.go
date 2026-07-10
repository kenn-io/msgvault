package packer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type maintenanceFixture struct {
	t     *testing.T
	store *store.Store
	dir   string
	msgID int64
	seq   int
}

type cancelAfterErrContext struct {
	context.Context

	calls       int
	cancelAfter int
}

func (c *cancelAfterErrContext) Err() error {
	c.calls++
	if c.calls >= c.cancelAfter {
		return context.Canceled
	}
	return nil
}

func newMaintenanceFixture(t *testing.T) *maintenanceFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err)
	convID, err := st.EnsureConversation(src.ID, "maintenance-thread", "Maintenance Thread")
	require.NoError(t, err)
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "maintenance-message",
		MessageType:     "email",
	})
	require.NoError(t, err)
	return &maintenanceFixture{t: t, store: st, dir: t.TempDir(), msgID: msgID}
}

func maintenanceHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func maintenanceCanonical(hash string) string { return hash[:2] + "/" + hash }

func (f *maintenanceFixture) addRow(hash, relPath string, size int) {
	f.t.Helper()
	f.seq++
	err := f.store.UpsertAttachment(f.msgID, fmt.Sprintf("maintenance-%d.bin", f.seq),
		"application/octet-stream", relPath, hash, size)
	require.NoError(f.t, err)
}

func (f *maintenanceFixture) write(relPath string, content []byte) string {
	f.t.Helper()
	path := filepath.Join(f.dir, filepath.FromSlash(relPath))
	require.NoError(f.t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(f.t, os.WriteFile(path, content, 0o600))
	return path
}

func (f *maintenanceFixture) addBlob(content []byte, relPath func(string) string) (string, string) {
	f.t.Helper()
	hash := maintenanceHash(content)
	rel := relPath(hash)
	f.addRow(hash, rel, len(content))
	return hash, f.write(rel, content)
}

func (f *maintenanceFixture) read(hash string) []byte {
	f.t.Helper()
	bs := blobstore.New(f.store, f.dir)
	defer func() { require.NoError(f.t, bs.Close()) }()
	r, _, err := bs.Open(hash)
	require.NoError(f.t, err)
	defer func() { require.NoError(f.t, r.Close()) }()
	data, err := io.ReadAll(r)
	require.NoError(f.t, err)
	return data
}

func (f *maintenanceFixture) storagePath(hash string) string {
	f.t.Helper()
	var path string
	err := f.store.DB().QueryRow(f.store.Rebind(`
		SELECT storage_path FROM attachments WHERE content_hash = ? LIMIT 1`), hash).Scan(&path)
	require.NoError(f.t, err)
	return path
}

func setMaintenanceTestLimits(t *testing.T, packEntries int) {
	t.Helper()
	oldBlobBytes := maintenanceBlobBytes
	oldPackEntries := maintenancePackEntries
	maintenanceBlobBytes = 1024
	maintenancePackEntries = packEntries
	t.Cleanup(func() {
		maintenanceBlobBytes = oldBlobBytes
		maintenancePackEntries = oldPackEntries
	})
}

func buildMaintenanceOrphan(t *testing.T, dir string, blobs ...[]byte) (string, string) {
	t.Helper()
	packsDir := filepath.Join(dir, "packs")
	require.NoError(t, os.MkdirAll(packsDir, 0o700))
	w, err := pack.NewWriter(packsDir, pack.WriterOptions{})
	require.NoError(t, err)
	for _, blob := range blobs {
		_, err := w.Append(blob)
		require.NoError(t, err)
	}
	id := w.ID()
	path := filepath.Join(packsDir, id[:2], id+blobstore.PackExt)
	_, err = w.Seal(path)
	require.NoError(t, err)
	return id, path
}

func captureMaintenanceLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestRunAcceptsLooseBlobAtMaintenanceLimit(t *testing.T) {
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1024)
	hash, _ := f.addBlob(content, maintenanceCanonical)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(t, err)
	assert.Equal(1, stats.BlobsPacked)
	assert.Zero(stats.BlobsDeferredOversized)
	assert.Equal(content, f.read(hash))
}

func TestRunDefersOversizedCanonicalBlobOncePerHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, path := f.addBlob(content, maintenanceCanonical)
	f.addRow(hash, maintenanceCanonical(hash), len(content))
	logs := captureMaintenanceLogs(t)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.Zero(stats.BlobsPacked)
	assert.FileExists(path)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.Nil(entry)
	assert.Equal(content, f.read(hash))
	assert.Contains(logs.String(), "hash="+hash)
	assert.Contains(logs.String(), "raw_bytes=1025")
	assert.Contains(logs.String(), "max_raw_bytes=1024")
}

func TestRunDefersCanonicalOversizedBlobWithoutOpeningIt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	want := make([]byte, 1025)
	want[0] = 1
	hash := maintenanceHash(want)
	path := f.write(maintenanceCanonical(hash), make([]byte, len(want)))
	f.addRow(hash, maintenanceCanonical(hash), len(want))
	oldOpen := openLooseFile
	var opens int
	openLooseFile = func(candidate string) (*os.File, error) {
		if candidate == path {
			opens++
			return nil, errors.New("oversized canonical file must not be opened")
		}
		return oldOpen(candidate)
	}
	t.Cleanup(func() { openLooseFile = oldOpen })

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Zero(opens)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.Zero(stats.BlobsCorrupt)
	assert.FileExists(path)
	assert.Equal(maintenanceCanonical(hash), f.storagePath(hash))
}

func TestRunMigratesOversizedNoncanonicalBlobWithoutPacking(t *testing.T) {
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	canonical := filepath.Join(f.dir, filepath.FromSlash(maintenanceCanonical(hash)))

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(t, err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.NoFileExists(legacy)
	assert.FileExists(canonical)
	assert.Equal(maintenanceCanonical(hash), f.storagePath(hash))
	assert.Equal(content, f.read(hash))
}

func TestRunRetainsOversizedLegacySourceUntilDatabaseCanonicalizes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	_, err := f.store.DB().Exec(f.store.Rebind(`
		CREATE TRIGGER block_blob_canonicalization
		BEFORE UPDATE OF storage_path ON attachments
		BEGIN SELECT RAISE(ABORT, 'injected canonicalization failure'); END`))
	require.NoError(err)

	_, err = Run(context.Background(), f.store, f.dir, Options{})
	require.Error(err)
	assert.FileExists(legacy, "legacy source remains authoritative after the DB failure")
	assert.Equal("legacy/"+hash, f.storagePath(hash))
	assert.FileExists(filepath.Join(f.dir, filepath.FromSlash(maintenanceCanonical(hash))),
		"a verified canonical copy may remain as harmless crash-recovery data")
}

func TestRunRetriesOversizedLegacyDeletionAfterCanonicalCommit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	oldRemove := removeLooseFile
	removeLooseFile = func(path string) error {
		if path == legacy {
			return errors.New("injected legacy removal failure")
		}
		return oldRemove(path)
	}
	t.Cleanup(func() { removeLooseFile = oldRemove })

	first, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, first.BlobsDeferredOversized)
	assert.FileExists(legacy)
	assert.Equal(maintenanceCanonical(hash), f.storagePath(hash))
	assert.Equal(content, f.read(hash))

	removeLooseFile = oldRemove
	second, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.NoFileExists(legacy)
	assert.Equal(1, second.LooseSwept)
	assert.Equal(content, f.read(hash))
}

func TestRunDefersWholeOrphanWhenAnyEntryIsOversized(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	small := make([]byte, 512)
	large := make([]byte, 1025)
	for _, content := range [][]byte{small, large} {
		hash := maintenanceHash(content)
		f.addRow(hash, maintenanceCanonical(hash), len(content))
	}
	id, path := buildMaintenanceOrphan(t, f.dir, small, large)
	logs := captureMaintenanceLogs(t)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.PacksDeferredOversized)
	assert.Zero(stats.PacksAdopted)
	assert.FileExists(path)
	has, err := f.store.HasPackRecord(id)
	require.NoError(err)
	assert.False(has)
	for _, content := range [][]byte{small, large} {
		entry, err := f.store.GetAttachmentPackEntry(maintenanceHash(content))
		require.NoError(err)
		assert.Nil(entry, "no entry from an oversized orphan may be adopted")
	}
	assert.Contains(logs.String(), "pack="+id)
	assert.Contains(logs.String(), "raw_bytes=1025")
	assert.Contains(logs.String(), "max_raw_bytes=1024")
	assert.Contains(logs.String(), "withheld_entries=2")
}

func TestRunAdoptsSmallReferencedCandidateDespiteDeadOversizedOrphanEntry(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	f := newMaintenanceFixture(t)
	small := []byte("small referenced orphan candidate")
	largeDead := make([]byte, 1025)
	smallHash := maintenanceHash(small)
	f.addRow(smallHash, maintenanceCanonical(smallHash), len(small))
	id, path := buildMaintenanceOrphan(t, f.dir, small, largeDead)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Zero(stats.PacksDeferredOversized)
	assert.Equal(1, stats.PacksAdopted)
	assert.FileExists(path)
	has, err := f.store.HasPackRecord(id)
	require.NoError(err)
	assert.True(has)
	entry, err := f.store.GetAttachmentPackEntry(smallHash)
	require.NoError(err)
	require.NotNil(entry)
	assert.Equal(id, entry.PackID)
	assert.Equal(small, f.read(smallHash))
}

func TestRunDefersWholeOrphanBeforeReadingCandidateWhenExistingCopyIsOversized(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	large := make([]byte, 1025)
	largeHash, _ := f.addBlob(large, maintenanceCanonical)
	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	existing, err := f.store.GetAttachmentPackEntry(largeHash)
	require.NoError(err)
	require.NotNil(existing)

	small := []byte("small candidate withheld by oversized redundancy check")
	smallHash := maintenanceHash(small)
	f.addRow(smallHash, maintenanceCanonical(smallHash), len(small))
	orphanID, orphanPath := buildMaintenanceOrphan(t, f.dir, small, large)
	r, err := blobstore.OpenMaintenancePack(orphanPath)
	require.NoError(err)
	var largeOffset int64
	for _, entry := range r.Entries() {
		if entry.ID.String() == largeHash {
			largeOffset = int64(entry.Offset)
		}
	}
	require.NoError(r.Close())
	require.Positive(largeOffset)
	packFile, err := os.OpenFile(orphanPath, os.O_RDWR, 0)
	require.NoError(err)
	var corrupt [1]byte
	_, err = packFile.ReadAt(corrupt[:], largeOffset)
	require.NoError(err)
	corrupt[0] ^= 0xff
	_, err = packFile.WriteAt(corrupt[:], largeOffset)
	require.NoError(err)
	require.NoError(packFile.Close())
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)
	logs := captureMaintenanceLogs(t)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Equal(1, stats.PacksDeferredOversized)
	assert.Zero(stats.PacksAdopted)
	assert.Zero(stats.PacksQuarantined, "the damaged orphan candidate must not be read")
	has, err := f.store.HasPackRecord(orphanID)
	require.NoError(err)
	assert.False(has)
	smallEntry, err := f.store.GetAttachmentPackEntry(smallHash)
	require.NoError(err)
	assert.Nil(smallEntry, "all candidates are withheld together")
	largeEntry, err := f.store.GetAttachmentPackEntry(largeHash)
	require.NoError(err)
	require.NotNil(largeEntry)
	assert.Equal(existing.PackID, largeEntry.PackID)
	assert.Contains(logs.String(), "raw_bytes=1025")
	assert.Contains(logs.String(), "max_raw_bytes=1024")
	assert.Contains(logs.String(), "withheld_entries=2")
}

func TestRunSealsWriterBeforeEntryCountLimitIsExceeded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	setMaintenanceTestLimits(t, 2)
	f := newMaintenanceFixture(t)
	for i := range 5 {
		content := fmt.Appendf(nil, "entry-count-%d", i)
		f.addBlob(content, maintenanceCanonical)
	}

	stats, err := Run(context.Background(), f.store, f.dir, Options{TargetSize: 1 << 20})
	require.NoError(err)
	assert.Equal(3, stats.PacksSealed)
	recs, err := f.store.ListPackRecords()
	require.NoError(err)
	require.Len(recs, 3)
	for _, rec := range recs {
		assert.LessOrEqual(rec.EntryCount, int64(2))
	}
}

func TestRunRecoversOversizedIndexedBlobFromLooseCopy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, _ := f.addBlob(content, maintenanceCanonical)
	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	require.NotNil(entry)
	legacy := f.write("legacy/"+hash, content)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)

	stats, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	assert.Zero(stats.BlobsPacked)
	assert.NoFileExists(legacy)
	entry, err = f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.Nil(entry, "unsafe oversized mapping is dropped only after canonical recovery")
	assert.Equal(content, f.read(hash))
}

func TestRunKeepsOversizedIndexWhenLooseRecoveryIsCorrupt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, _ := f.addBlob(content, maintenanceCanonical)
	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	legacy := f.write("legacy/"+hash, []byte("corrupt recovery bytes"))
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)

	_, err = Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.NotNil(entry)
	assert.FileExists(legacy)
	assert.Equal(content, f.read(hash), "the still-readable packed copy remains authoritative")
}

func TestRunReportsDatabaseFailureDuringOversizedSweepRecovery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := make([]byte, 1025)
	hash, _ := f.addBlob(content, maintenanceCanonical)
	_, err := Run(context.Background(), f.store, f.dir, Options{})
	require.NoError(err)
	legacyRel := "legacy/" + hash
	legacy := f.write(legacyRel, content)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		UPDATE attachments SET storage_path = ? WHERE content_hash = ?`), legacyRel, hash)
	require.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		CREATE TRIGGER block_sweep_canonicalization
		BEFORE UPDATE OF storage_path ON attachments
		BEGIN SELECT RAISE(ABORT, 'injected sweep canonicalization failure'); END`))
	require.NoError(err)
	setMaintenanceTestLimits(t, blobstore.MaxMaintenancePackEntries)

	_, err = Run(context.Background(), f.store, f.dir, Options{})
	require.Error(err, "a systemic DB failure must not be downgraded to a damaged-content warning")
	assert.FileExists(legacy)
	entry, lookupErr := f.store.GetAttachmentPackEntry(hash)
	require.NoError(lookupErr)
	assert.NotNil(entry, "the mapping stays authoritative when canonicalization does not commit")
}

func TestCanonicalizeLooseSourceStopsBeforeDatabaseCommitWhenContextCancels(t *testing.T) {
	assert := assert.New(t)
	f := newMaintenanceFixture(t)
	content := []byte("context cancellation after streaming copy")
	hash, legacy := f.addBlob(content, func(hash string) string { return "legacy/" + hash })
	ctx := &cancelAfterErrContext{Context: context.Background(), cancelAfter: 3}

	err := canonicalizeLooseSource(ctx, f.store, f.dir, hash, legacy)
	require.ErrorIs(t, err, context.Canceled)
	assert.FileExists(legacy, "source deletion must follow a successful DB commit")
	assert.Equal("legacy/"+hash, f.storagePath(hash))
	assert.FileExists(canonicalLoosePath(f.dir, hash),
		"a fully published canonical copy is harmless recovery data")
}

func TestPublishLooseFallbackNeverClobbersRacingDestination(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	staging := filepath.Join(dir, "blob.staging")
	canonical := filepath.Join(dir, "canonical")
	require.NoError(os.WriteFile(staging, []byte("new canonical bytes"), 0o600))
	oldLink := linkLooseFile
	oldCreate := createExclusiveLooseFile
	linkLooseFile = func(_, _ string) error { return errors.New("hard links unsupported") }
	createExclusiveLooseFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		require.Equal(canonical, name)
		require.NoError(os.WriteFile(canonical, []byte("racing writer bytes"), 0o600))
		return oldCreate(name, flag, perm)
	}
	t.Cleanup(func() {
		linkLooseFile = oldLink
		createExclusiveLooseFile = oldCreate
	})

	err := publishLooseNoClobber(staging, canonical)
	require.ErrorIs(err, fs.ErrExist)
	got, err := os.ReadFile(canonical)
	require.NoError(err)
	assert.Equal([]byte("racing writer bytes"), got)
	assert.FileExists(staging, "failed publication retains the verified staging source")
}
